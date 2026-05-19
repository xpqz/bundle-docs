//go:build fts5

package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/xpqz/bundle-docs/internal/semanticstore"
)

func TestSemanticSearchCommandSupportsFTSModeAndFetch(t *testing.T) {
	exe := buildDocsearchForSearch(t)
	dbPath := seedSemanticSearchDB(t)

	search := exec.Command(exe, "-d", dbPath, "-s", "⎕FIX", "-semantic-mode", "fts", "-l", "5")
	out, err := search.CombinedOutput()
	if err != nil {
		t.Fatalf("semantic FTS search: %v\n%s", err, out)
	}
	output := string(out)
	for _, want := range []string{"Fix Script", "Language / Fix", "⎕FIX", "fts#1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("semantic FTS output missing %q:\n%s", want, output)
		}
	}

	id := firstResultID(t, output)
	fetch := exec.Command(exe, "-d", dbPath, "-r", id)
	fetched, err := fetch.CombinedOutput()
	if err != nil {
		t.Fatalf("fetch semantic chunk: %v\n%s", err, fetched)
	}
	if !strings.Contains(string(fetched), "⎕FIX fixes a script") {
		t.Fatalf("fetch output = %q", fetched)
	}
}

func TestSemanticFetchPrefersChunkWhenDocRowIDCollides(t *testing.T) {
	exe := buildDocsearchForSearch(t)
	dbPath := seedSemanticSearchDB(t)
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open collision db: %v", err)
	}
	execCommandSearchSQL(t, db, `
		CREATE TABLE docs (
			path TEXT PRIMARY KEY,
			file TEXT NOT NULL,
			title TEXT NOT NULL,
			keywords TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL,
			exclude INTEGER NOT NULL DEFAULT 0
		)
	`)
	execCommandSearchSQL(t, db, `
		INSERT INTO docs(rowid, path, file, title, content)
		VALUES (1, 'Legacy / Wrong', 'wrong.md', 'Wrong', 'wrong legacy document')
	`)
	if err := db.Close(); err != nil {
		t.Fatalf("close collision db: %v", err)
	}

	fetch := exec.Command(exe, "-d", dbPath, "-r", "1")
	fetched, err := fetch.CombinedOutput()
	if err != nil {
		t.Fatalf("fetch collision row: %v\n%s", err, fetched)
	}
	if !strings.Contains(string(fetched), "⎕FIX fixes a script") || strings.Contains(string(fetched), "wrong legacy document") {
		t.Fatalf("fetch collision output = %q", fetched)
	}
}

func TestSemanticSearchVectorModeRequiresVectorExtension(t *testing.T) {
	exe := buildDocsearchForSearch(t)
	dbPath := seedSemanticSearchDB(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()

	search := exec.Command(exe, "-d", dbPath, "-s", "how do I define a namespace", "-semantic-mode", "vector", "-embedding-url", server.URL, "-vector-dims", "3")
	search.Env = append(os.Environ(), "DOCSEARCH_VECTOR_EXTENSION=", "HOME="+t.TempDir())
	out, err := search.CombinedOutput()
	if err == nil {
		t.Fatalf("semantic vector search without sqlite-vec succeeded:\n%s", out)
	}
	if !strings.Contains(string(out), "-vector-extension") {
		t.Fatalf("vector error = %q, want vector extension guidance", out)
	}
}

func TestSemanticSearchHybridModeSupportsFallbackForDeterministicTests(t *testing.T) {
	exe := buildDocsearchForSearch(t)
	dbPath := seedSemanticSearchDB(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string   `json:"model"`
			Texts []string `json:"texts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		resp := struct {
			Model      string      `json:"model"`
			Dimensions int         `json:"dimensions"`
			Embeddings [][]float32 `json:"embeddings"`
		}{
			Model:      req.Model,
			Dimensions: 3,
			Embeddings: [][]float32{{0, 1, 0}},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	search := exec.Command(exe, "-d", dbPath, "-s", "namespace references", "-semantic-mode", "hybrid", "-embedding-url", server.URL, "-vector-dims", "3", "-semantic-test-fallback-vector")
	out, err := search.CombinedOutput()
	if err != nil {
		t.Fatalf("semantic hybrid search: %v\n%s", err, out)
	}
	output := string(out)
	for _, want := range []string{"Namespaces", "Language / Namespaces", "Namespace references", "fts#", "vector#"} {
		if !strings.Contains(output, want) {
			t.Fatalf("semantic hybrid output missing %q:\n%s", want, output)
		}
	}
}

func buildDocsearchForSearch(t *testing.T) string {
	t.Helper()

	exe := filepath.Join(t.TempDir(), "docsearch")
	build := exec.Command("go", "build", "-tags", "fts5", "-o", exe, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build docsearch: %v\n%s", err, out)
	}
	return exe
}

func seedSemanticSearchDB(t *testing.T) string {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "semantic.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	if err := semanticstore.EnsureCoreSchema(db); err != nil {
		t.Fatalf("ensure semantic schema: %v", err)
	}
	execCommandSearchSQL(t, db, `CREATE TABLE chunk_vec(rowid INTEGER PRIMARY KEY, embedding TEXT NOT NULL)`)
	insertCommandSearchChunk(t, db, "Language / Fix", "Fix Script", "⎕FIX", "⎕FIX fixes a script into the workspace.", "[1,0,0]")
	insertCommandSearchChunk(t, db, "Language / Namespaces", "Namespaces", "Namespace references", "Namespace references explain how to define and resolve namespaces.", "[0,1,0]")
	return dbPath
}

func insertCommandSearchChunk(t *testing.T, db *sql.DB, path, title, heading, text, vector string) {
	t.Helper()

	docID, err := semanticstore.UpsertDocument(db, semanticstore.Document{Path: path, Title: title})
	if err != nil {
		t.Fatalf("upsert document: %v", err)
	}
	chunkID, err := semanticstore.UpsertChunk(db, semanticstore.Chunk{
		DocumentID:  docID,
		Ordinal:     int(docID - 1),
		Heading:     heading,
		Anchor:      strings.ToLower(strings.ReplaceAll(heading, " ", "-")),
		Text:        text,
		ContentHash: text,
	})
	if err != nil {
		t.Fatalf("upsert chunk: %v", err)
	}
	execCommandSearchSQL(t, db, `INSERT INTO chunks_fts(rowid, title, heading, text) VALUES (?, ?, ?, ?)`, chunkID, title, heading, text)
	execCommandSearchSQL(t, db, semanticstore.VectorUpsertSQL(), chunkID, vector)
}

func execCommandSearchSQL(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()

	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
}

func firstResultID(t *testing.T, output string) string {
	t.Helper()

	match := regexp.MustCompile(`(?m)^([0-9]+)\s`).FindStringSubmatch(output)
	if match == nil {
		t.Fatalf("could not find result id in output:\n%s", output)
	}
	return match[1]
}
