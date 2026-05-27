//go:build semantic

package semanticstore

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func openMemoryDB(t *testing.T) *sql.DB {
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
	return db
}

func TestEnsureCoreSchemaCreatesRepeatableHybridStorage(t *testing.T) {
	db := openMemoryDB(t)

	if err := EnsureCoreSchema(db); err != nil {
		t.Fatalf("ensure core schema: %v", err)
	}
	if err := EnsureCoreSchema(db); err != nil {
		t.Fatalf("ensure core schema twice: %v", err)
	}

	execSQL(t, db, `INSERT INTO documents(path, title) VALUES ('language/symbols/iota.md', 'Index Generator')`)
	execSQL(t, db, `
		INSERT INTO chunks(id, document_id, ordinal, heading, anchor, text, content_hash)
		VALUES (
			101,
			(SELECT id FROM documents WHERE path = 'language/symbols/iota.md'),
			0,
			'R gets index generator Y',
			'index-generator',
			'The iota primitive generates indices for arrays.',
			'abc123'
		)
	`)
	execSQL(t, db, `
		INSERT INTO chunks_fts(rowid, title, heading, text)
		SELECT c.id, d.title, c.heading, c.text
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE c.id = 101
	`)

	var rowid int64
	var title string
	err := db.QueryRow(`
		SELECT f.rowid, f.title
		FROM chunks_fts f
		WHERE chunks_fts MATCH 'iota'
	`).Scan(&rowid, &title)
	if err != nil {
		t.Fatalf("query chunks_fts: %v", err)
	}
	if rowid != 101 {
		t.Fatalf("FTS rowid = %d, want stable chunks.id 101", rowid)
	}
	if title != "Index Generator" {
		t.Fatalf("FTS title = %q, want %q", title, "Index Generator")
	}
}

func TestCoreFTSPreservesExactAPLGlyphLookups(t *testing.T) {
	db := openMemoryDB(t)

	if err := EnsureCoreSchema(db); err != nil {
		t.Fatalf("ensure core schema: %v", err)
	}

	docID, err := UpsertDocument(db, Document{
		Path:  "language/system-functions/fix.md",
		Title: "Fix Script",
	})
	if err != nil {
		t.Fatalf("upsert document: %v", err)
	}
	chunkID, err := UpsertChunk(db, Chunk{
		DocumentID:  docID,
		Ordinal:     0,
		Heading:     "⎕FIX",
		Anchor:      "fix",
		Text:        "⎕FIX fixes a script into the workspace. :If can control execution. ⍴ reports shape. ← assigns a value.",
		ContentHash: "fix-v1",
	})
	if err != nil {
		t.Fatalf("upsert chunk: %v", err)
	}
	execSQL(t, db, `
		INSERT INTO chunks_fts(rowid, title, heading, text)
		SELECT c.id, d.title, c.heading, c.text
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE c.id = ?
	`, chunkID)

	for _, query := range []string{"⎕FIX", ":If", "⍴", "←"} {
		t.Run(query, func(t *testing.T) {
			var got int64
			if err := db.QueryRow(`SELECT rowid FROM chunks_fts WHERE chunks_fts MATCH ?`, quoteFTS(query)).Scan(&got); err != nil {
				t.Fatalf("query %q: %v", query, err)
			}
			if got != chunkID {
				t.Fatalf("query %q rowid = %d, want chunk id %d", query, got, chunkID)
			}
		})
	}
}

func TestChunkIdentityIsStableAcrossContentReindex(t *testing.T) {
	db := openMemoryDB(t)

	if err := EnsureCoreSchema(db); err != nil {
		t.Fatalf("ensure core schema: %v", err)
	}

	docID, err := UpsertDocument(db, Document{
		Path:  "programming/namespaces.md",
		Title: "Namespaces",
	})
	if err != nil {
		t.Fatalf("upsert document: %v", err)
	}
	firstID, err := UpsertChunk(db, Chunk{
		DocumentID:  docID,
		Ordinal:     2,
		Heading:     "Namespace references",
		Anchor:      "namespace-references",
		Text:        "Namespace references are resolved at evaluation time.",
		ContentHash: "v1",
	})
	if err != nil {
		t.Fatalf("first upsert chunk: %v", err)
	}
	secondID, err := UpsertChunk(db, Chunk{
		DocumentID:  docID,
		Ordinal:     2,
		Heading:     "Namespace references",
		Anchor:      "namespace-references",
		Text:        "Namespace references remain the same chunk after content changes.",
		ContentHash: "v2",
	})
	if err != nil {
		t.Fatalf("second upsert chunk: %v", err)
	}
	if secondID != firstID {
		t.Fatalf("updated chunk id = %d, want stable id %d", secondID, firstID)
	}

	var text, hash string
	if err := db.QueryRow(`SELECT text, content_hash FROM chunks WHERE id = ?`, firstID).Scan(&text, &hash); err != nil {
		t.Fatalf("read updated chunk: %v", err)
	}
	if text != "Namespace references remain the same chunk after content changes." || hash != "v2" {
		t.Fatalf("updated chunk = (%q, %q), want changed text and hash", text, hash)
	}
}

func TestDeleteDocumentRemovesChunksAndFTSRows(t *testing.T) {
	db := openMemoryDB(t)

	if err := EnsureCoreSchema(db); err != nil {
		t.Fatalf("ensure core schema: %v", err)
	}

	docID, err := UpsertDocument(db, Document{
		Path:  "old/path.md",
		Title: "Moved Document",
	})
	if err != nil {
		t.Fatalf("upsert document: %v", err)
	}
	chunkID, err := UpsertChunk(db, Chunk{
		DocumentID:  docID,
		Ordinal:     0,
		Heading:     "Old Heading",
		Anchor:      "old-heading",
		Text:        "Text that should leave FTS after deletion.",
		ContentHash: "v1",
	})
	if err != nil {
		t.Fatalf("upsert chunk: %v", err)
	}
	execSQL(t, db, `
		INSERT INTO chunks_fts(rowid, title, heading, text)
		SELECT c.id, d.title, c.heading, c.text
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE c.id = ?
	`, chunkID)

	if err := DeleteDocument(db, "old/path.md"); err != nil {
		t.Fatalf("delete document: %v", err)
	}

	var chunkCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chunks WHERE id = ?`, chunkID).Scan(&chunkCount); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if chunkCount != 0 {
		t.Fatalf("chunks after DeleteDocument = %d, want 0", chunkCount)
	}

	var ftsCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chunks_fts WHERE rowid = ?`, chunkID).Scan(&ftsCount); err != nil {
		t.Fatalf("count FTS rows: %v", err)
	}
	if ftsCount != 0 {
		t.Fatalf("FTS rows after DeleteDocument = %d, want 0", ftsCount)
	}
}

func TestDeleteDocumentRemovesVectorRowsWhenVectorTableExists(t *testing.T) {
	db := openMemoryDB(t)

	if err := EnsureCoreSchema(db); err != nil {
		t.Fatalf("ensure core schema: %v", err)
	}
	execSQL(t, db, `CREATE TABLE chunk_vec(rowid INTEGER PRIMARY KEY, embedding BLOB)`)

	docID, err := UpsertDocument(db, Document{
		Path:  "deleted/vector.md",
		Title: "Deleted Vector Document",
	})
	if err != nil {
		t.Fatalf("upsert document: %v", err)
	}
	chunkID, err := UpsertChunk(db, Chunk{
		DocumentID:  docID,
		Ordinal:     0,
		Heading:     "Vector row",
		Anchor:      "vector-row",
		Text:        "Vector row should be deleted with the document.",
		ContentHash: "v1",
	})
	if err != nil {
		t.Fatalf("upsert chunk: %v", err)
	}
	execSQL(t, db, `INSERT INTO chunk_vec(rowid, embedding) VALUES (?, ?)`, chunkID, []byte{1, 2, 3})

	if err := DeleteDocument(db, "deleted/vector.md"); err != nil {
		t.Fatalf("delete document: %v", err)
	}

	var vectorCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chunk_vec WHERE rowid = ?`, chunkID).Scan(&vectorCount); err != nil {
		t.Fatalf("count vector rows: %v", err)
	}
	if vectorCount != 0 {
		t.Fatalf("vector rows after DeleteDocument = %d, want 0", vectorCount)
	}
}

func TestVectorSQLUsesChunkRowIDAndDefaultDimensions(t *testing.T) {
	createSQL := VectorSchemaSQL(VectorConfig{Dimensions: DefaultEmbeddingDimensions})
	if !strings.Contains(createSQL, "chunk_vec") {
		t.Fatalf("VectorSchemaSQL = %q, want chunk_vec table", createSQL)
	}
	if !strings.Contains(createSQL, "FLOAT[384]") {
		t.Fatalf("VectorSchemaSQL = %q, want 384-dimensional embeddings", createSQL)
	}

	insertSQL := VectorUpsertSQL()
	if !strings.Contains(insertSQL, "rowid") {
		t.Fatalf("VectorUpsertSQL = %q, want explicit rowid insert for chunks.id mapping", insertSQL)
	}
}

func TestVectorSchemaRequiresLoadedVec0Extension(t *testing.T) {
	db := openMemoryDB(t)

	if err := EnsureCoreSchema(db); err != nil {
		t.Fatalf("ensure core schema: %v", err)
	}

	err := EnsureVectorSchema(db, VectorConfig{Dimensions: DefaultEmbeddingDimensions})
	if !errors.Is(err, ErrVectorExtensionUnavailable) {
		t.Fatalf("EnsureVectorSchema error = %v, want ErrVectorExtensionUnavailable", err)
	}
	if err == nil || !strings.Contains(err.Error(), "sqlite-vec") {
		t.Fatalf("EnsureVectorSchema error = %v, want sqlite-vec guidance", err)
	}
}

func TestLoadVectorExtensionReportsMissingExtension(t *testing.T) {
	db := openMemoryDB(t)

	err := LoadVectorExtension(db, "/definitely/missing/sqlite-vec")
	if !errors.Is(err, ErrVectorExtensionUnavailable) {
		t.Fatalf("LoadVectorExtension error = %v, want ErrVectorExtensionUnavailable", err)
	}
	if err == nil || !strings.Contains(err.Error(), "/definitely/missing/sqlite-vec") {
		t.Fatalf("LoadVectorExtension error = %v, want path context", err)
	}
}

func TestLoadVectorExtensionCanEnableVectorSchemaWhenConfigured(t *testing.T) {
	extensionPath := os.Getenv("SQLITE_VEC_EXTENSION")
	if extensionPath == "" {
		t.Skip("set SQLITE_VEC_EXTENSION to a sqlite-vec loadable extension for integration coverage")
	}

	db := openMemoryDB(t)
	if err := EnsureCoreSchema(db); err != nil {
		t.Fatalf("ensure core schema: %v", err)
	}
	if err := LoadVectorExtension(db, extensionPath); err != nil {
		t.Fatalf("load vector extension: %v", err)
	}
	if err := EnsureVectorSchema(db, VectorConfig{Dimensions: DefaultEmbeddingDimensions}); err != nil {
		t.Fatalf("ensure vector schema: %v", err)
	}
}

func execSQL(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()

	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
}

func quoteFTS(q string) string {
	return `"` + strings.ReplaceAll(q, `"`, `""`) + `"`
}

func TestSetMetaAndGetMetaRoundTrip(t *testing.T) {
	db := openMemoryDB(t)

	got, ok, err := GetMeta(db, "embedding_model")
	if err != nil {
		t.Fatalf("GetMeta on missing table: %v", err)
	}
	if ok || got != "" {
		t.Fatalf("expected ok=false on missing table; got ok=%v val=%q", ok, got)
	}

	if err := SetMeta(db, "embedding_model", "BAAI/bge-small-en-v1.5"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	got, ok, err = GetMeta(db, "embedding_model")
	if err != nil || !ok || got != "BAAI/bge-small-en-v1.5" {
		t.Fatalf("after set: got=%q ok=%v err=%v", got, ok, err)
	}

	// Idempotent: rewriting the same key updates value, not creates dupes.
	if err := SetMeta(db, "embedding_model", "other-model"); err != nil {
		t.Fatalf("SetMeta (update): %v", err)
	}
	got, _, _ = GetMeta(db, "embedding_model")
	if got != "other-model" {
		t.Fatalf("update should overwrite, got %q", got)
	}

	// Missing key in existing table: ok=false, no error.
	got, ok, err = GetMeta(db, "no-such-key")
	if err != nil || ok || got != "" {
		t.Fatalf("missing key: got=%q ok=%v err=%v", got, ok, err)
	}
}

// TestOpenWithVectorAllowsConcurrentReaders confirms the
// ConnectHook-backed driver lets multiple goroutines hold their
// own connections at the same time, which is what
// docsearch serve needs under live-search load.
func TestOpenWithVectorAllowsConcurrentReaders(t *testing.T) {
	extensionPath := os.Getenv("SQLITE_VEC_PATH")
	if extensionPath == "" {
		t.Skip("SQLITE_VEC_PATH not set; skipping concurrent reader test")
	}
	dbPath := filepath.Join(t.TempDir(), "concurrent.db")

	db, err := OpenWithVector(dbPath, extensionPath)
	if err != nil {
		t.Fatalf("OpenWithVector: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)

	if err := EnsureCoreSchema(db); err != nil {
		t.Fatalf("core schema: %v", err)
	}
	if err := EnsureVectorSchema(db, VectorConfig{Dimensions: DefaultEmbeddingDimensions}); err != nil {
		t.Fatalf("vec schema: %v", err)
	}

	// Hold a connection open in one goroutine, run another query
	// on a second goroutine. With MaxOpenConns(1) the second would
	// block until the first releases; with ConnectHook + 4-pool
	// both proceed concurrently.
	holdReady := make(chan struct{})
	holdRelease := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		conn, err := db.Conn(t.Context())
		if err != nil {
			t.Errorf("acquire conn 1: %v", err)
			close(holdReady)
			return
		}
		defer conn.Close()
		var n int
		if err := conn.QueryRowContext(t.Context(), "SELECT 1").Scan(&n); err != nil {
			t.Errorf("conn1 query: %v", err)
		}
		close(holdReady)
		<-holdRelease
	}()

	go func() {
		defer wg.Done()
		<-holdReady
		var version string
		if err := db.QueryRow("SELECT vec_version()").Scan(&version); err != nil {
			t.Errorf("vec_version while conn1 holds: %v", err)
		}
		if version == "" {
			t.Errorf("expected non-empty vec_version, got empty")
		}
		close(holdRelease)
	}()

	wg.Wait()
}

// TestRegisterVectorDriverRejectsConflictingPath protects callers
// from accidentally re-registering the global driver name with a
// different extension path.
func TestRegisterVectorDriverRejectsConflictingPath(t *testing.T) {
	extensionPath := os.Getenv("SQLITE_VEC_PATH")
	if extensionPath == "" {
		t.Skip("SQLITE_VEC_PATH not set; skipping driver re-registration test")
	}
	if err := RegisterVectorDriver(extensionPath); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	// Re-registering with the same path is fine (no-op).
	if err := RegisterVectorDriver(extensionPath); err != nil {
		t.Fatalf("second registration with same path: %v", err)
	}
	// Different path should error.
	other := filepath.Join(t.TempDir(), "bogus-vec.dylib")
	if err := os.WriteFile(other, []byte{0}, 0o644); err != nil {
		t.Fatalf("write fake ext: %v", err)
	}
	err := RegisterVectorDriver(other)
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("expected conflict error, got %v", err)
	}
}
