// docsearch queries the Dyalog documentation database.
//
//	go build -tags "fts5" -o docsearch docsearch.go
package main

import (
	"bufio"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

const (
	envEmbeddingURL    = "DOCSEARCH_EMBEDDING_URL"
	envVectorExtension = "DOCSEARCH_VECTOR_EXTENSION"
	defaultEmbeddingURL = "http://localhost:8000/embed"
)

func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "dyalog-docs.db"
	}
	return filepath.Join(home, ".bundle-docs", "dyalog-docs.db")
}

func defaultEmbeddingURLValue() string {
	if v := os.Getenv(envEmbeddingURL); v != "" {
		return v
	}
	return defaultEmbeddingURL
}

func defaultVectorExtension() string {
	if v := os.Getenv(envVectorExtension); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	var name string
	switch runtime.GOOS {
	case "darwin":
		name = "vec0.dylib"
	case "windows":
		name = "vec0.dll"
	default:
		name = "vec0.so"
	}
	candidate := filepath.Join(home, ".bundle-docs", name)
	if _, err := os.Stat(candidate); err != nil {
		return ""
	}
	return candidate
}

func main() {
	if maybeRunSemanticIndex(os.Args) {
		return
	}
	if maybeRunServe(os.Args) {
		return
	}

	dbPath := flag.String("d", defaultDBPath(), "database path")
	search := flag.String("s", "", "search string (use '-' to read from stdin)")
	rowid := flag.Int64("r", 0, "fetch document by rowid")
	limit := flag.Int("l", 10, "maximum number of results")
	semanticMode := flag.String("semantic-mode", "", "semantic search mode: fts, vector, or hybrid")
	embeddingURL := flag.String("embedding-url", defaultEmbeddingURLValue(), "local embedding HTTP endpoint (env: "+envEmbeddingURL+")")
	embeddingModel := flag.String("embedding-model", "BAAI/bge-small-en-v1.5", "embedding model name for semantic search")
	vectorExtension := flag.String("vector-extension", defaultVectorExtension(), "sqlite-vec loadable extension path (env: "+envVectorExtension+")")
	vectorDims := flag.Int("vector-dims", 384, "semantic embedding dimensions")
	semanticFallbackVector := flag.Bool("semantic-test-fallback-vector", false, "use JSON vector fallback for tests")
	flag.Parse()

	if *search == "" && *rowid == 0 {
		fmt.Fprintf(os.Stderr, `docsearch - search the Dyalog APL documentation database

This tool provides full-text search over the Dyalog APL documentation.
It covers language features, primitive functions and operators, system
functions, GUI references, and more.

USAGE
  docsearch -s <query>          Search for documents matching <query>
  docsearch -r <rowid>          Fetch the full content of a document by its rowid
  docsearch semantic-index      Build or refresh the semantic chunk/vector index

WORKFLOW
  Searching is a two-step process:

  1. Search by keyword to get a list of matching documents:
       docsearch -s "index generator"
     Output (one per line): ROWID TITLE
       86 Index Generator R←⍳Y
       502 Search Functions and Hash Tables

  2. Fetch the full document content using the rowid from step 1:
       docsearch -r 86
     Output: the complete document text (markdown).

FLAGS
  -s <string>    Search query. Matches against keywords, titles, and content
                  (in that priority order). Use '-' to read from stdin.
  -r <rowid>     Fetch and print the full content of the document with this rowid.
  -l <limit>     Maximum number of search results (default: 10).
  -d <path>      Path to the SQLite database (default: %s).
  -semantic-mode <mode>
                 Semantic search mode: fts, vector, or hybrid.
  -embedding-url <url>
                 Local embedding HTTP endpoint for vector or hybrid search.
                 Default: $%s if set, else %s.
  -vector-extension <path>
                 sqlite-vec loadable extension path for vector or hybrid search.
                 Default: $%s if set, else ~/.bundle-docs/vec0.{dylib,so,dll}.

SEMANTIC INDEX
  docsearch semantic-index -d <db> -embedding-url <url> -vector-extension <path>

  The semantic-index mode chunks Markdown documents by heading, sends chunk
  text to a local embedding service, and stores semantic documents, chunks,
  FTS rows, and vector rows in SQLite. The default embedding model is
  BAAI/bge-small-en-v1.5.

  Semantic index flags:
    -embedding-url <url>      Local embedding HTTP endpoint.
    -embedding-model <name>   Embedding model name (default: BAAI/bge-small-en-v1.5).
    -vector-extension <path>  sqlite-vec loadable extension path.
    -batch-size <n>           Texts per embedding request.
    -vector-dims <n>          Embedding dimensions.

SEARCH BEHAVIOUR
  Results are ranked in three tiers and deduplicated:
    1. Exact case-insensitive match on hidden search keywords
    2. FTS5 match on document title
    3. FTS5 match on document content
  The query is matched as a phrase. Use natural language terms, APL symbol
  names, or system function names (e.g. "⍳", "iota", "quad-IO", "⎕IO").

DATABASE
  The database is generated by bundle-docs. Run "bundle-docs update" to
  refresh it from the upstream Dyalog documentation repository.
`, defaultDBPath(), envEmbeddingURL, defaultEmbeddingURL, envVectorExtension)
		os.Exit(1)
	}

	db, err := sql.Open("sqlite3", *dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if *rowid != 0 {
		fetchByRowid(db, *rowid)
		return
	}

	query := *search
	if query == "-" {
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			query = scanner.Text()
		}
		if err := scanner.Err(); err != nil {
			log.Fatal(err)
		}
	}

	if query == "" {
		log.Fatal("empty search string")
	}

	if *semanticMode != "" {
		runSemanticSearch(db, query, *semanticMode, *embeddingURL, *embeddingModel, *vectorExtension, *vectorDims, *semanticFallbackVector, *limit)
		return
	}

	searchDocs(db, query, *limit)
}

func fetchByRowid(db *sql.DB, rowid int64) {
	var content string
	if semanticTablesExist(db) {
		err := db.QueryRow("SELECT text FROM chunks WHERE id = ?", rowid).Scan(&content)
		if err == nil {
			fmt.Print(content)
			return
		}
		if err != sql.ErrNoRows {
			log.Fatal(err)
		}
	}
	err := db.QueryRow("SELECT content FROM docs WHERE rowid = ?", rowid).Scan(&content)
	if err != nil {
		if err != sql.ErrNoRows && !strings.Contains(err.Error(), "no such table: docs") {
			log.Fatal(err)
		}
		if err := db.QueryRow("SELECT text FROM chunks WHERE id = ?", rowid).Scan(&content); err != nil {
			if err == sql.ErrNoRows {
				log.Fatalf("no document or semantic chunk with rowid %d", rowid)
			}
			log.Fatal(err)
		}
	}
	fmt.Print(content)
}

func semanticTablesExist(db *sql.DB) bool {
	var count int
	if err := db.QueryRow(`
		SELECT COUNT(*)
		FROM sqlite_schema
		WHERE name IN ('documents', 'chunks')
	`).Scan(&count); err != nil {
		return false
	}
	return count == 2
}

func searchDocs(db *sql.DB, query string, limit int) {
	seen := make(map[int64]bool)
	count := 0

	// 1. Exact case-insensitive match on keywords
	rows, err := db.Query(`
		SELECT rowid, title FROM docs
		WHERE keywords LIKE ? COLLATE NOCASE AND exclude = 0
	`, "%"+query+"%")
	if err != nil {
		log.Fatal(err)
	}
	count = printResults(rows, seen, limit, count)
	if count >= limit {
		return
	}

	// 2. FTS search on title
	rows, err = db.Query(`
		SELECT f.rowid, f.title FROM docs_fts f
		JOIN docs d ON f.rowid = d.rowid
		WHERE f.title MATCH ? AND d.exclude = 0
	`, escapeQuery(query))
	if err != nil {
		log.Fatal(err)
	}
	count = printResults(rows, seen, limit, count)
	if count >= limit {
		return
	}

	// 3. FTS search on content
	rows, err = db.Query(`
		SELECT f.rowid, f.title FROM docs_fts f
		JOIN docs d ON f.rowid = d.rowid
		WHERE f.content MATCH ? AND d.exclude = 0
	`, escapeQuery(query))
	if err != nil {
		log.Fatal(err)
	}
	printResults(rows, seen, limit, count)
}

func printResults(rows *sql.Rows, seen map[int64]bool, limit, count int) int {
	defer rows.Close()
	for rows.Next() {
		if count >= limit {
			break
		}
		var rowid int64
		var title string
		if err := rows.Scan(&rowid, &title); err != nil {
			log.Fatal(err)
		}
		if seen[rowid] {
			continue
		}
		seen[rowid] = true
		fmt.Printf("%d %s\n", rowid, title)
		count++
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}
	return count
}

// escapeQuery wraps the query in quotes to handle special characters
func escapeQuery(q string) string {
	// Escape double quotes by doubling them
	q = strings.ReplaceAll(q, `"`, `""`)
	return `"` + q + `"`
}
