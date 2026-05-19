//go:build fts5

package semanticindex

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/xpqz/bundle-docs/internal/semanticstore"
)

type fakeEmbedder struct {
	calls [][]string
}

func (f *fakeEmbedder) Embed(ctx context.Context, texts []string) (EmbeddingBatch, error) {
	f.calls = append(f.calls, append([]string(nil), texts...))
	embeddings := make([][]float32, len(texts))
	for i := range texts {
		embeddings[i] = []float32{float32(i + 1), float32(len(texts)), 0}
	}
	return EmbeddingBatch{
		Model:      DefaultEmbeddingModel,
		Dimensions: 3,
		Embeddings: embeddings,
	}, nil
}

type failingEmbedder struct{}

func (f failingEmbedder) Embed(ctx context.Context, texts []string) (EmbeddingBatch, error) {
	return EmbeddingBatch{}, errors.New("embedding service unavailable")
}

type wrongDimensionEmbedder struct{}

func (w wrongDimensionEmbedder) Embed(ctx context.Context, texts []string) (EmbeddingBatch, error) {
	return EmbeddingBatch{
		Model:      DefaultEmbeddingModel,
		Dimensions: 2,
		Embeddings: [][]float32{{1, 2}},
	}, nil
}

func TestIndexDatabasePopulatesDocumentsChunksFTSAndVectors(t *testing.T) {
	db := openIndexDB(t)
	seedDocsTable(t, db)
	insertDoc(t, db, "Language / Functions", "language/functions.md", "Functions", "# Functions\n\nIntro.\n\n## Tradfns\n\nTradfn body.\n\n## Dfns\n\nDfn body.", false)
	insertDoc(t, db, "Hidden / Helper", "hidden.md", "Hidden", "# Hidden\n\nDo not index.", true)

	embedder := &fakeEmbedder{}
	stats, err := IndexDatabase(context.Background(), db, embedder, IndexOptions{
		Chunk:               ChunkOptions{MaxTokens: 80},
		BatchSize:           2,
		VectorDims:          3,
		UseFallbackVectorDB: true,
	})
	if err != nil {
		t.Fatalf("index database: %v", err)
	}
	if stats.Documents != 1 || stats.Chunks != 3 || stats.Embeddings != 3 {
		t.Fatalf("stats = %#v, want 1 document, 3 chunks, 3 embeddings", stats)
	}
	if len(embedder.calls) != 2 {
		t.Fatalf("embedder calls = %d, want 2 batches", len(embedder.calls))
	}

	assertCount(t, db, `SELECT COUNT(*) FROM documents`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM chunks`, 3)
	assertCount(t, db, `SELECT COUNT(*) FROM chunks_fts`, 3)
	assertCount(t, db, `SELECT COUNT(*) FROM chunk_vec`, 3)

	var rowid int64
	if err := db.QueryRow(`SELECT rowid FROM chunks_fts WHERE chunks_fts MATCH '"Tradfns"'`).Scan(&rowid); err != nil {
		t.Fatalf("query FTS: %v", err)
	}

	var vector string
	if err := db.QueryRow(`SELECT embedding FROM chunk_vec WHERE rowid = ?`, rowid).Scan(&vector); err != nil {
		t.Fatalf("query vector row: %v", err)
	}
	var decoded []float32
	if err := json.Unmarshal([]byte(vector), &decoded); err != nil {
		t.Fatalf("decode vector JSON %q: %v", vector, err)
	}
	if len(decoded) != 3 {
		t.Fatalf("vector dimensions = %d, want 3", len(decoded))
	}
}

func TestIndexDatabaseEmbedsTitleAndHeadingWithChunkBody(t *testing.T) {
	db := openIndexDB(t)
	seedDocsTable(t, db)
	// Mimics a primitive function page: the canonical name lives in title
	// and heading, the body never mentions "Execute". The embedder must
	// receive all three so vector search can match natural-language
	// queries against the page.
	insertDoc(t, db, "Language / Execute", "language/execute.md", "Execute R←⍎Y", "# Execute R←⍎Y\n\nWarning: untrusted input is risky.", false)

	embedder := &fakeEmbedder{}
	_, err := IndexDatabase(context.Background(), db, embedder, IndexOptions{
		Chunk:               ChunkOptions{MaxTokens: 80},
		BatchSize:           10,
		VectorDims:          3,
		UseFallbackVectorDB: true,
	})
	if err != nil {
		t.Fatalf("index database: %v", err)
	}
	if len(embedder.calls) != 1 || len(embedder.calls[0]) != 1 {
		t.Fatalf("embedder calls = %#v, want one batch of one text", embedder.calls)
	}
	embedded := embedder.calls[0][0]
	for _, want := range []string{"Execute R←⍎Y", "untrusted input"} {
		if !strings.Contains(embedded, want) {
			t.Fatalf("embedded text missing %q:\n%s", want, embedded)
		}
	}
}

func TestIndexDatabaseReportsEmbeddingFailures(t *testing.T) {
	db := openIndexDB(t)
	seedDocsTable(t, db)
	insertDoc(t, db, "Language / Errors", "language/errors.md", "Errors", "# Errors\n\nTrap errors.", false)

	_, err := IndexDatabase(context.Background(), db, failingEmbedder{}, IndexOptions{
		Chunk:               ChunkOptions{MaxTokens: 80},
		BatchSize:           10,
		VectorDims:          3,
		UseFallbackVectorDB: true,
	})
	if err == nil || !strings.Contains(err.Error(), "embedding service unavailable") {
		t.Fatalf("IndexDatabase error = %v, want embedding service context", err)
	}
}

func TestIndexDatabaseRejectsWrongVectorDimensions(t *testing.T) {
	db := openIndexDB(t)
	seedDocsTable(t, db)
	insertDoc(t, db, "Language / Shape", "language/shape.md", "Shape", "# Shape\n\nShape text.", false)

	_, err := IndexDatabase(context.Background(), db, wrongDimensionEmbedder{}, IndexOptions{
		Chunk:               ChunkOptions{MaxTokens: 80},
		BatchSize:           10,
		VectorDims:          3,
		UseFallbackVectorDB: true,
	})
	if err == nil || !strings.Contains(err.Error(), "embedding dimensions") {
		t.Fatalf("IndexDatabase error = %v, want dimension mismatch", err)
	}
}

func TestIndexDatabaseCanBeRerunWithoutDuplicatingRows(t *testing.T) {
	db := openIndexDB(t)
	seedDocsTable(t, db)
	insertDoc(t, db, "Language / Namespaces", "language/namespaces.md", "Namespaces", "# Namespaces\n\nIntro.\n\n## References\n\nReferences body.", false)

	embedder := &fakeEmbedder{}
	options := IndexOptions{
		Chunk:               ChunkOptions{MaxTokens: 80},
		BatchSize:           10,
		VectorDims:          3,
		UseFallbackVectorDB: true,
	}
	first, err := IndexDatabase(context.Background(), db, embedder, options)
	if err != nil {
		t.Fatalf("first index: %v", err)
	}
	firstIDs := readChunkIDs(t, db)

	execIndexSQL(t, db, `UPDATE docs SET content = ? WHERE path = ?`, "# Namespaces\n\nIntro changed.", "Language / Namespaces")

	second, err := IndexDatabase(context.Background(), db, embedder, options)
	if err != nil {
		t.Fatalf("second index: %v", err)
	}
	if second.Documents != first.Documents {
		t.Fatalf("second documents = %d, first = %d", second.Documents, first.Documents)
	}
	secondIDs := readChunkIDs(t, db)
	if len(secondIDs) != 1 || secondIDs[0] != firstIDs[0] {
		t.Fatalf("chunk ids after content shrink = %#v, want only original first id %#v", secondIDs, firstIDs[0])
	}

	assertCount(t, db, `SELECT COUNT(*) FROM documents`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM chunks`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM chunks_fts`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM chunk_vec`, 1)

	var text string
	if err := db.QueryRow(`SELECT text FROM chunks WHERE id = ?`, firstIDs[0]).Scan(&text); err != nil {
		t.Fatalf("read updated chunk text: %v", err)
	}
	if text != "Intro changed." {
		t.Fatalf("updated chunk text = %q, want Intro changed.", text)
	}
}

func openIndexDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close sqlite: %v", err)
		}
	})

	if err := semanticstore.EnsureCoreSchema(db); err != nil {
		t.Fatalf("ensure semantic schema: %v", err)
	}
	execIndexSQL(t, db, `CREATE TABLE chunk_vec(rowid INTEGER PRIMARY KEY, embedding TEXT NOT NULL)`)
	return db
}

func seedDocsTable(t *testing.T, db *sql.DB) {
	t.Helper()

	execIndexSQL(t, db, `
		CREATE TABLE docs (
			path TEXT PRIMARY KEY,
			file TEXT NOT NULL,
			title TEXT NOT NULL,
			keywords TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL,
			exclude INTEGER NOT NULL DEFAULT 0
		)
	`)
}

func insertDoc(t *testing.T, db *sql.DB, path, file, title, content string, exclude bool) {
	t.Helper()

	excludeValue := 0
	if exclude {
		excludeValue = 1
	}
	execIndexSQL(t, db, `
		INSERT INTO docs(path, file, title, content, exclude)
		VALUES (?, ?, ?, ?, ?)
	`, path, file, title, content, excludeValue)
}

func assertCount(t *testing.T, db *sql.DB, query string, want int) {
	t.Helper()

	var got int
	if err := db.QueryRow(query).Scan(&got); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	if got != want {
		t.Fatalf("%s = %d, want %d", query, got, want)
	}
}

func execIndexSQL(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()

	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
}

func readChunkIDs(t *testing.T, db *sql.DB) []int64 {
	t.Helper()

	rows, err := db.Query(`SELECT id FROM chunks ORDER BY id`)
	if err != nil {
		t.Fatalf("query chunk ids: %v", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan chunk id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate chunk ids: %v", err)
	}
	return ids
}
