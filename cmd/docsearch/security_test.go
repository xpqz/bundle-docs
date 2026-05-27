//go:build semantic

package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"

	"github.com/xpqz/bundle-docs/internal/semanticstore"
)

// TestRenderMarkdownSanitizesDangerousHTML verifies that the chunk-
// render pipeline strips XSS-y bits even though goldmark is
// configured with WithUnsafe.
func TestRenderMarkdownSanitizesDangerousHTML(t *testing.T) {
	s := newTestServer(t)

	cases := []struct {
		name     string
		input    string
		mustHave []string // substrings that should survive
		mustNot  []string // substrings that must not appear
	}{
		{
			name:    "script tag stripped",
			input:   "Hello <script>alert('xss')</script> world",
			mustHave: []string{"Hello", "world"},
			mustNot: []string{"<script", "alert('xss')"},
		},
		{
			name:    "iframe stripped",
			input:   "Look: <iframe src=\"https://evil.example/\"></iframe>",
			mustHave: []string{"Look:"},
			mustNot: []string{"<iframe", "evil.example"},
		},
		{
			name:    "img onerror handler stripped",
			input:   `<img src="x" onerror="alert(1)" alt="boom">`,
			mustNot: []string{"onerror", "alert(1)"},
		},
		{
			name:    "javascript: href neutered",
			input:   `<a href="javascript:alert(1)">click</a>`,
			mustHave: []string{"click"},
			mustNot: []string{"javascript:", "alert(1)"},
		},
		{
			name:    "svg-with-script stripped",
			input:   `<svg><script>alert(1)</script></svg>`,
			mustNot: []string{"<script", "alert(1)"},
		},
		{
			name:     "table content survives",
			input:    "| a | b |\n|---|---|\n| 1 | 2 |\n",
			mustHave: []string{"<table>", "<td>", "1", "2"},
		},
		{
			name:     "fenced code survives",
			input:    "```apl\n⍳5\n```",
			mustHave: []string{"<pre>", "<code", "⍳5"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := s.renderMarkdown(tc.input, "")
			for _, want := range tc.mustHave {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q in:\n%s", want, out)
				}
			}
			for _, banned := range tc.mustNot {
				if strings.Contains(out, banned) {
					t.Fatalf("contained banned %q in:\n%s", banned, out)
				}
			}
		})
	}
}

func TestSecurityHeadersAreSetOnAllRoutes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/foo", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "yes"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><html></html>"))
	})
	ts := httptest.NewServer(securityHeaders(mux))
	defer ts.Close()

	cases := []struct {
		path        string
		wantCSPHas  []string
		wantCSPMiss []string
	}{
		{
			path:        "/",
			wantCSPHas:  []string{"default-src 'self'", "frame-ancestors 'none'", "base-uri 'none'"},
			wantCSPMiss: []string{"default-src 'none'"},
		},
		{
			path:        "/api/foo",
			wantCSPHas:  []string{"default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'"},
			wantCSPMiss: []string{"unsafe-inline"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()
			for _, h := range []string{"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy", "Content-Security-Policy"} {
				if resp.Header.Get(h) == "" {
					t.Fatalf("missing header %s on %s", h, tc.path)
				}
			}
			csp := resp.Header.Get("Content-Security-Policy")
			for _, want := range tc.wantCSPHas {
				if !strings.Contains(csp, want) {
					t.Fatalf("%s: CSP missing %q (got %q)", tc.path, want, csp)
				}
			}
			for _, miss := range tc.wantCSPMiss {
				if strings.Contains(csp, miss) {
					t.Fatalf("%s: CSP unexpectedly contains %q (got %q)", tc.path, miss, csp)
				}
			}
			if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
				t.Fatalf("X-Frame-Options = %q, want DENY", got)
			}
		})
	}
}

func TestHandleSearchRejectsOverlongQuery(t *testing.T) {
	s := newTestServer(t)
	long := strings.Repeat("x", maxQueryLength+1)
	req := httptest.NewRequest(http.MethodGet, "/api/search?q="+long, nil)
	w := httptest.NewRecorder()
	s.handleSearch(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "characters or fewer") {
		t.Fatalf("body = %q, want length-limit message", body)
	}
}

func TestHandleChunkDiagnosticsAreClear(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		path     string
		wantCode int
		wantMsg  string
	}{
		{"/api/chunk/", http.StatusBadRequest, "missing chunk id"},
		{"/api/chunk/notanint", http.StatusBadRequest, "invalid chunk id"},
		{"/api/chunk/-3", http.StatusBadRequest, "invalid chunk id"},
		{"/api/chunk/../etc/passwd", http.StatusBadRequest, "invalid chunk id"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			s.handleChunk(w, req)
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantCode)
			}
			if !strings.Contains(w.Body.String(), tc.wantMsg) {
				t.Fatalf("body = %q, want substring %q", w.Body.String(), tc.wantMsg)
			}
		})
	}
}

func TestHandleHealthDoesNotLeakEmbeddingURL(t *testing.T) {
	s := newTestServer(t)
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
	if _, leaked := body["embedding_url"]; leaked {
		t.Fatalf("/api/health leaked embedding_url: %#v", body)
	}
	if _, ok := body["vector_ready"]; !ok {
		t.Fatalf("/api/health missing vector_ready: %#v", body)
	}
}

func TestWriteServerErrorDoesNotEchoUnderlyingError(t *testing.T) {
	w := httptest.NewRecorder()
	writeServerError(w, "search", &leakyError{msg: "table users does not exist"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	if bytes.Contains(body, []byte("table users")) {
		t.Fatalf("500 response leaked internal error: %s", body)
	}
	if !bytes.Contains(body, []byte("internal server error")) {
		t.Fatalf("500 body = %s, want generic message", body)
	}
}

type leakyError struct{ msg string }

func (e *leakyError) Error() string { return e.msg }

// newTestServer wires up just enough of the server struct for unit
// tests that don't need a populated DB. The minimal docs/meta tables
// are created and seeded with one row so the smart healthcheck
// doesn't 503 in tests that don't care about it.
func newTestServer(t *testing.T) *server {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := semanticstore.EnsureCoreSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS docs(path TEXT, file TEXT, title TEXT, keywords TEXT, content TEXT, exclude INTEGER);
		INSERT INTO docs(path, file, title, keywords, content, exclude)
		VALUES ('test/path', 'test.md', 'Test', '', 'body', 0);
	`); err != nil {
		t.Fatalf("seed docs: %v", err)
	}
	return &server{
		db:          db,
		vectorDims:  384,
		vectorReady: true,
		md: goldmark.New(
			goldmark.WithExtensions(extension.GFM),
			goldmark.WithParserOptions(parser.WithAutoHeadingID()),
			goldmark.WithRendererOptions(html.WithUnsafe()),
		),
	}
}
