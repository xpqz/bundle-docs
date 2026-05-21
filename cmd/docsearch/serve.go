//go:build semantic

package main

import (
	"bytes"
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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"

	"github.com/xpqz/bundle-docs/internal/semanticindex"
	"github.com/xpqz/bundle-docs/internal/semanticsearch"
	"github.com/xpqz/bundle-docs/internal/semanticstore"
)

// maxQueryLength caps the q parameter on /api/search. The search box
// in the UI realistically generates queries well under 100 chars; a
// 10 KB query would otherwise burn a full embedder round-trip on
// every keystroke.
const maxQueryLength = 256

// chunkHTMLPolicy is the allowlist applied to rendered chunk HTML
// before it is returned to the browser. Without this, goldmark's
// html.WithUnsafe() option lets any raw HTML in the upstream
// markdown pass straight through; the corpus already contains a few
// <script> tags in prose (chunk #1883 has them documented as text,
// not in a code fence) and is sourced from a third-party repo, so
// a single bad PR upstream would otherwise be enough to land XSS in
// every replica's chunk panel.
//
// Built on bluemonday.UGCPolicy which already permits paragraphs,
// lists, code, pre, blockquotes, headings, and tables; strips
// <script>, <iframe>, <object>, <embed>, all on* event handlers,
// and javascript:/vbscript: URLs. We add the few extras the
// chunker actually emits: class attributes on heading/code/table
// elements (mkdocs uses them for styling), target+rel on anchors
// (so the rewritten dyalog.github.io links can keep target=_blank).
var chunkHTMLPolicy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("class").OnElements(
		"code", "pre", "span", "div",
		"table", "thead", "tbody", "tr", "td", "th",
		"h1", "h2", "h3", "h4", "h5", "h6",
		"blockquote", "ul", "ol", "li", "p",
	)
	p.AllowAttrs("colspan", "rowspan").OnElements("td", "th")
	p.AllowAttrs("target", "rel").OnElements("a")
	// Our rewriteRelativeLinks already sets rel="noopener"; don't
	// let bluemonday clobber it with its own value.
	p.RequireNoFollowOnLinks(false)
	return p
}()

// dyalogDocsBase is the public host where the Dyalog mkdocs site is
// published. A chunk's source URL is formed by stripping the leading
// "<subsite>/docs/" segment and the trailing ".md" from the file path,
// then prepending this base. The chunk's anchor (a slug derived from
// its heading) is appended as the URL fragment so the link jumps to
// the right section.
const dyalogDocsBase = "https://dyalog.github.io/documentation/20.0"

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
		db:          db,
		vectorDims:  *vectorDims,
		vectorReady: vectorReady,
		embedder:    semanticindex.HTTPEmbeddingClient{URL: *embeddingURL, Model: *embeddingModel},
		md: goldmark.New(
			goldmark.WithExtensions(extension.GFM),
			goldmark.WithParserOptions(parser.WithAutoHeadingID()),
			goldmark.WithRendererOptions(html.WithUnsafe()),
		),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", srv.handleSearch)
	mux.HandleFunc("/api/chunk/", srv.handleChunk)
	mux.HandleFunc("/api/health", srv.handleHealth)
	mux.HandleFunc("/api/version", srv.handleVersion)
	mux.HandleFunc("/", srv.handleIndex)

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB; Go default, made explicit.
	}

	log.Printf("docsearch serve listening on http://%s (db=%s, vector_ready=%v)", *addr, *dbPath, vectorReady)
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

type server struct {
	db          *sql.DB
	vectorDims  int
	vectorReady bool
	embedder    semanticindex.Embedder
	md          goldmark.Markdown
}

// sourceURL builds the canonical help.dyalog page URL for a chunk.
//
// docs.file looks like
//
//	language-reference-guide/docs/primitive-functions/tally.md
//
// the published site lives at
//
//	https://dyalog.github.io/documentation/20.0/language-reference-guide/primitive-functions/tally
//
// so we strip the "/docs/" segment and the ".md" suffix. Empty file
// returns "" so callers can skip the link in the UI.
func sourceURL(file, anchor string) string {
	file = strings.TrimSpace(file)
	if file == "" {
		return ""
	}
	file = strings.TrimSuffix(file, ".md")
	if idx := strings.Index(file, "/docs/"); idx >= 0 {
		file = file[:idx] + file[idx+len("/docs"):]
	}
	url := dyalogDocsBase + "/" + strings.TrimPrefix(file, "/")
	if anchor = strings.TrimSpace(anchor); anchor != "" {
		url += "#" + anchor
	}
	return url
}

// renderMarkdown converts the chunk text to HTML. mkdocs admonitions
// are converted to blockquotes so they survive a plain CommonMark
// renderer, and relative `.md` cross-reference links are rewritten to
// absolute help.dyalog.com URLs. The result is then passed through
// bluemonday so any raw HTML in upstream markdown that goldmark
// would otherwise let through (under html.WithUnsafe) is sanitized
// before reaching the client. On failure we fall back to a
// pre-formatted block of the raw text so the UI still shows something
// usable. sourceFile is the chunk's docs.file (may be empty for
// databases without the docs table); when empty, cross-ref rewriting
// is skipped.
func (s *server) renderMarkdown(text, sourceFile string) string {
	if s.md == nil {
		return "<pre>" + htmlEscape(text) + "</pre>"
	}
	var buf bytes.Buffer
	if err := s.md.Convert([]byte(preprocessAdmonitions(text)), &buf); err != nil {
		return "<pre>" + htmlEscape(text) + "</pre>"
	}
	rendered := rewriteRelativeLinks(buf.String(), sourceFile)
	return chunkHTMLPolicy.Sanitize(rendered)
}

// mdLinkRE finds href="<path>.md" and href="<path>.md#anchor".
// Non-greedy capture so we don't span across multiple attributes.
var mdLinkRE = regexp.MustCompile(`href="([^"]+?)\.md(#[^"]*)?"`)

// rewriteRelativeLinks turns relative .md hrefs inside the rendered
// HTML into absolute help.dyalog.com URLs, computed by resolving the
// link target against the source file's directory. Absolute URLs are
// passed through unchanged.
func rewriteRelativeLinks(htmlStr, sourceFile string) string {
	if sourceFile == "" {
		return htmlStr
	}
	sourceDir := path.Dir(sourceFile)
	return mdLinkRE.ReplaceAllStringFunc(htmlStr, func(match string) string {
		m := mdLinkRE.FindStringSubmatch(match)
		if m == nil {
			return match
		}
		target := m[1]
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "//") || strings.HasPrefix(target, "mailto:") {
			return match
		}
		resolved := path.Clean(path.Join(sourceDir, target)) + ".md"
		anchor := ""
		if len(m) > 2 && m[2] != "" {
			anchor = strings.TrimPrefix(m[2], "#")
		}
		url := sourceURL(resolved, anchor)
		if url == "" {
			return match
		}
		return `href="` + url + `" target="_blank" rel="noopener"`
	})
}

// admonitionRE matches a mkdocs admonition opener like:
//
//	!!! note
//	!!! Warning "Be careful"
//
// Capture groups: (1) admonition type, (2) optional explicit title.
var admonitionRE = regexp.MustCompile(`^!!!\s+(\w+)(?:\s+"([^"]*)")?\s*$`)

// preprocessAdmonitions rewrites mkdocs-style admonitions into plain
// Markdown blockquotes. The continuation block (indented 4 spaces or a
// tab) is unindented and prefixed with "> " so it remains part of the
// quote and is not treated as a code block by CommonMark. A blank line
// stays inside the blockquote only when another indented line follows;
// otherwise it terminates the admonition.
func preprocessAdmonitions(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	i := 0
	for i < len(lines) {
		m := admonitionRE.FindStringSubmatch(lines[i])
		if m == nil {
			out = append(out, lines[i])
			i++
			continue
		}
		title := m[2]
		if title == "" {
			t := m[1]
			if len(t) > 0 {
				title = strings.ToUpper(t[:1]) + t[1:]
			}
		}
		out = append(out, "> **"+title+"**")
		i++
		for i < len(lines) {
			line := lines[i]
			switch {
			case strings.HasPrefix(line, "    "):
				out = append(out, "> "+line[4:])
				i++
			case strings.HasPrefix(line, "\t"):
				out = append(out, "> "+line[1:])
				i++
			case strings.TrimSpace(line) == "" && hasMoreIndented(lines, i+1):
				out = append(out, ">")
				i++
			default:
				// Either a non-indented line or a blank line
				// with no further indented continuation. Stop
				// consuming - the outer loop will emit it.
				goto admonitionDone
			}
		}
	admonitionDone:
	}
	return strings.Join(out, "\n")
}

func hasMoreIndented(lines []string, start int) bool {
	for j := start; j < len(lines); j++ {
		line := lines[j]
		if strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
			return true
		}
		if strings.TrimSpace(line) != "" {
			return false
		}
	}
	return false
}

func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}

// securityHeaders wraps the mux to add a baseline set of response
// headers. The CSP is strict for JSON APIs (default-src 'none', the
// browser shouldn't render these as documents anyway) and looser for
// the HTML page so its inline <style> and <script> blocks still run.
// 'unsafe-inline' on script-src is the cost of keeping the page a
// single embedded file; the chunk-render XSS surface is already
// covered upstream by bluemonday so the CSP here is defense in depth.
func securityHeaders(next http.Handler) http.Handler {
	const apiCSP = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'"
	const htmlCSP = "default-src 'self'; " +
		"script-src 'self' 'unsafe-inline'; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; " +
		"connect-src 'self'; " +
		"frame-ancestors 'none'; " +
		"base-uri 'none'; " +
		"form-action 'self'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Content-Security-Policy", apiCSP)
		} else {
			h.Set("Content-Security-Policy", htmlCSP)
		}
		next.ServeHTTP(w, r)
	})
}

// writeServerError logs the real error server-side and returns a
// generic message to the client. Lets us tell the difference between
// "we want to surface this validation failure to the user"
// (writeJSONError) and "something went wrong internally that the
// user can't act on and we shouldn't disclose".
func writeServerError(w http.ResponseWriter, op string, err error) {
	log.Printf("docsearch serve: %s: %v", op, err)
	writeJSONError(w, http.StatusInternalServerError, "internal server error")
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	// vector_ready is enough for the UI; embedding_url is operator
	// configuration and gratuitous to leak.
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       "ok",
		"vector_ready": s.vectorReady,
	})
}

func (s *server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, collectVersionInfo(s.db))
}

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSONError(w, http.StatusBadRequest, "missing q parameter")
		return
	}
	if len(q) > maxQueryLength {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("q must be %d characters or fewer", maxQueryLength))
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
		Query:            q,
		Mode:             semanticsearch.Mode(mode),
		Limit:            limit,
		VectorDims:       s.vectorDims,
		DedupeByDocument: true,
	})
	if err != nil {
		writeServerError(w, "search", err)
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
		SourceURL   string  `json:"source_url,omitempty"`
	}
	out := make([]item, len(results))
	for i, r := range results {
		file, anchor := s.lookupChunkLocation(ctx, r.ChunkID)
		out[i] = item{
			ChunkID:     r.ChunkID,
			Title:       r.Title,
			Path:        r.Path,
			Heading:     r.Heading,
			Snippet:     r.Snippet,
			Score:       r.Score,
			Source:      string(r.Source),
			Explanation: r.Explanation,
			SourceURL:   sourceURL(file, anchor),
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query":   q,
		"mode":    mode,
		"limit":   limit,
		"results": out,
	})
}

// lookupChunkLocation joins chunks → documents → docs to get the
// repo-relative file path for a chunk. Returns empty strings if the
// docs row is missing (older databases without the bundle-docs
// `docs` table will simply omit source_url).
func (s *server) lookupChunkLocation(ctx context.Context, chunkID int64) (file, anchor string) {
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(src.file, ''), COALESCE(c.anchor, '')
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		LEFT JOIN docs src ON src.path = d.path
		WHERE c.id = ?
	`, chunkID).Scan(&file, &anchor)
	if err != nil {
		return "", ""
	}
	return file, anchor
}

func (s *server) handleChunk(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/chunk/")
	if rest == "" {
		writeJSONError(w, http.StatusBadRequest, "missing chunk id")
		return
	}
	if strings.ContainsRune(rest, '/') {
		writeJSONError(w, http.StatusBadRequest, "invalid chunk id")
		return
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid chunk id")
		return
	}
	var (
		chunkID int64
		title   string
		heading string
		anchor  string
		docPath string
		text    string
		file    string
	)
	err = s.db.QueryRowContext(r.Context(), `
		SELECT c.id, d.title, c.heading, c.anchor, d.path, c.text, COALESCE(src.file, '')
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		LEFT JOIN docs src ON src.path = d.path
		WHERE c.id = ?
	`, id).Scan(&chunkID, &title, &heading, &anchor, &docPath, &text, &file)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("chunk %d not found", id))
		return
	}
	if err != nil {
		writeServerError(w, "lookup chunk", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chunk_id":   chunkID,
		"title":      title,
		"heading":    heading,
		"anchor":     anchor,
		"path":       docPath,
		"text":       text,
		"html":       s.renderMarkdown(text, file),
		"source_url": sourceURL(file, anchor),
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
