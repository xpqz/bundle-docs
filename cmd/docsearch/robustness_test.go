//go:build semantic

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/xpqz/bundle-docs/internal/semanticstore"
)

func TestSanityCheckDBRejectsEmptyDocsTable(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	// docs table exists but is empty.
	if _, err := db.Exec(`CREATE TABLE docs(path TEXT, file TEXT, title TEXT, keywords TEXT, content TEXT, exclude INTEGER)`); err != nil {
		t.Fatalf("create docs: %v", err)
	}
	err = sanityCheckDB(db)
	if err == nil || !strings.Contains(err.Error(), "docs table is empty") {
		t.Fatalf("expected empty-docs error, got %v", err)
	}
}

func TestSanityCheckDBToleratesMissingChunksTable(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE docs(path TEXT, file TEXT, title TEXT, keywords TEXT, content TEXT, exclude INTEGER); INSERT INTO docs VALUES('p','f','t','','c',0);`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// No chunks table - older DB. Should still pass.
	if err := sanityCheckDB(db); err != nil {
		t.Fatalf("unexpected sanity-check failure: %v", err)
	}
}

func TestSanityCheckDBFailsWhenDocsTableMissing(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	err = sanityCheckDB(db)
	if err == nil {
		t.Fatalf("expected error on missing docs table")
	}
}

func TestHandleHealthReports503WhenDBUnreachable(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Close immediately so subsequent queries fail.
	db.Close()

	s := &server{db: db, vectorReady: true}
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	w := httptest.NewRecorder()
	s.handleHealth(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "down" {
		t.Errorf("status = %v, want down", body["status"])
	}
	if body["db"] != "error" {
		t.Errorf("db = %v, want error", body["db"])
	}
}

func TestHandleHealthReportsDegradedWhenVectorNotReady(t *testing.T) {
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := semanticstore.EnsureCoreSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	mustExec(t, db, `CREATE TABLE docs(path TEXT, file TEXT, title TEXT, keywords TEXT, content TEXT, exclude INTEGER); INSERT INTO docs VALUES('p','f','t','','c',0);`)

	s := &server{db: db, vectorReady: false}
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	w := httptest.NewRecorder()
	s.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "degraded" {
		t.Errorf("status = %v, want degraded when vector_ready=false", body["status"])
	}
	if body["vector_ready"] != false {
		t.Errorf("vector_ready = %v", body["vector_ready"])
	}
}

func TestAccessLogAttachesRequestIDHeader(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/echo", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"hi": "there"})
	})
	ts := httptest.NewServer(accessLog(mux))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/echo")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if id := resp.Header.Get(requestIDHeader); id == "" {
		t.Fatalf("missing %s header", requestIDHeader)
	} else if len(id) < 6 {
		t.Errorf("request id looks too short: %q", id)
	}
}

func TestRecoverPanicsReturnsCleanFiveHundred(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/boom", func(_ http.ResponseWriter, _ *http.Request) {
		panic("kaboom: containing a secret /etc/passwd")
	})
	// Same order as production: accessLog wraps the writer, then
	// recoverPanics sees the wrapped writer so it can detect that
	// headers haven't been written and emit a clean 500.
	ts := httptest.NewServer(accessLog(recoverPanics(mux)))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/boom")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "kaboom") || strings.Contains(string(body), "passwd") {
		t.Fatalf("panic detail leaked to client: %s", body)
	}
	if !strings.Contains(string(body), "internal server error") {
		t.Fatalf("body = %s, want generic 500", body)
	}
}

func TestHandleChunkRespectsRequestContextDeadline(t *testing.T) {
	db, _ := sql.Open("sqlite3", ":memory:")
	defer db.Close()
	if err := semanticstore.EnsureCoreSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	s := newTestServer(t)
	s.db = db

	// Build a request whose context has already been cancelled to
	// confirm the handler unwinds cleanly instead of running the
	// query unbounded.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/chunk/1", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	s.handleChunk(w, req)
	if w.Code == 0 || w.Code == http.StatusOK {
		t.Fatalf("status = %d; expected handler to short-circuit with cancelled context", w.Code)
	}
}
