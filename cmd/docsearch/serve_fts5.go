//go:build fts5

package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/xpqz/bundle-docs/internal/semanticindex"
	"github.com/xpqz/bundle-docs/internal/semanticsearch"
	"github.com/xpqz/bundle-docs/internal/semanticstore"
)

//go:embed static/index.html
var indexHTML []byte

func maybeRunServe(args []string) bool {
	if len(args) <= 1 || args[1] != "serve" {
		return false
	}
	runServe(args[2:])
	return true
}

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dbPath := fs.String("d", defaultDBPath(), "database path")
	addr := fs.String("addr", "127.0.0.1:8080", "HTTP listen address")
	embeddingURL := fs.String("embedding-url", defaultEmbeddingURLValue(), "local embedding HTTP endpoint (env: "+envEmbeddingURL+")")
	embeddingModel := fs.String("embedding-model", semanticindex.DefaultEmbeddingModel, "embedding model name")
	vectorExtension := fs.String("vector-extension", defaultVectorExtension(), "sqlite-vec loadable extension path (env: "+envVectorExtension+")")
	vectorDims := fs.Int("vector-dims", 384, "embedding dimensions")
	if err := fs.Parse(args); err != nil {
		log.Fatal(err)
	}

	db, err := sql.Open("sqlite3", *dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	vectorReady := false
	if *vectorExtension != "" {
		if err := semanticstore.LoadVectorExtension(db, *vectorExtension); err != nil {
			log.Printf("WARN: sqlite-vec not loaded (%v); vector/hybrid modes will fail until the extension is available", err)
		} else {
			vectorReady = true
		}
	} else {
		log.Printf("WARN: no -vector-extension or %s; only -semantic-mode fts will work", envVectorExtension)
	}

	srv := &server{
		db:           db,
		vectorDims:   *vectorDims,
		vectorReady:  vectorReady,
		embedder:     semanticindex.HTTPEmbeddingClient{URL: *embeddingURL, Model: *embeddingModel},
		embeddingURL: *embeddingURL,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", srv.handleSearch)
	mux.HandleFunc("/api/chunk/", srv.handleChunk)
	mux.HandleFunc("/api/health", srv.handleHealth)
	mux.HandleFunc("/", srv.handleIndex)

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("docsearch serve listening on http://%s (db=%s, vector_ready=%v)", *addr, *dbPath, vectorReady)
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

type server struct {
	db           *sql.DB
	vectorDims   int
	vectorReady  bool
	embedder     semanticindex.Embedder
	embeddingURL string
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"vector_ready":  s.vectorReady,
		"embedding_url": s.embeddingURL,
	})
}

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSONError(w, http.StatusBadRequest, "missing q parameter")
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = string(semanticsearch.ModeHybrid)
	}
	if !semanticsearch.IsSemanticMode(mode) {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("unknown mode %q (use fts, vector, or hybrid)", mode))
		return
	}

	limit := 10
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > 50 {
			writeJSONError(w, http.StatusBadRequest, "limit must be an integer in [1, 50]")
			return
		}
		limit = parsed
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	results, err := semanticsearch.SearchChunks(ctx, s.db, s.embedder, semanticsearch.SearchOptions{
		Query:      q,
		Mode:       semanticsearch.Mode(mode),
		Limit:      limit,
		VectorDims: s.vectorDims,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type item struct {
		ChunkID     int64   `json:"chunk_id"`
		Title       string  `json:"title"`
		Path        string  `json:"path"`
		Heading     string  `json:"heading"`
		Snippet     string  `json:"snippet"`
		Score       float64 `json:"score"`
		Source      string  `json:"source"`
		Explanation string  `json:"explanation"`
	}
	out := make([]item, len(results))
	for i, r := range results {
		out[i] = item{
			ChunkID:     r.ChunkID,
			Title:       r.Title,
			Path:        r.Path,
			Heading:     r.Heading,
			Snippet:     r.Snippet,
			Score:       r.Score,
			Source:      string(r.Source),
			Explanation: r.Explanation,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query":   q,
		"mode":    mode,
		"limit":   limit,
		"results": out,
	})
}

func (s *server) handleChunk(w http.ResponseWriter, r *http.Request) {
	idStr := path.Base(r.URL.Path)
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid chunk id")
		return
	}
	var out struct {
		ChunkID int64  `json:"chunk_id"`
		Title   string `json:"title"`
		Heading string `json:"heading"`
		Anchor  string `json:"anchor"`
		Path    string `json:"path"`
		Text    string `json:"text"`
	}
	err = s.db.QueryRowContext(r.Context(), `
		SELECT c.id, d.title, c.heading, c.anchor, d.path, c.text
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE c.id = ?
	`, id).Scan(&out.ChunkID, &out.Title, &out.Heading, &out.Anchor, &out.Path, &out.Text)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("chunk %d not found", id))
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
