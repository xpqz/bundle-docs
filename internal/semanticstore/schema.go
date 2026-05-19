//go:build fts5

package semanticstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/mattn/go-sqlite3"
)

const DefaultEmbeddingDimensions = 384

var ErrVectorExtensionUnavailable = errors.New("vector extension unavailable")

const aplTokenChars = "⎕⍺⍵⍴⍳⍸⍷⍨⍥⍣⍤⍞⍝⍙⍕⍔⍒⍋⍉⍀⌿⍠⍟⌸⌺⌶⌹⌷⌈⌊∇¯¨∘⌾⍛⍜⊢⊣×÷⌽⊖⍪↑↓⊂⊃∊⍬⍎←⋄○∨∧∩∪~?!/\\,.:"

type VectorConfig struct {
	Dimensions int
}

type Document struct {
	Path  string
	Title string
}

type Chunk struct {
	DocumentID  int64
	Ordinal     int
	Heading     string
	Anchor      string
	Text        string
	ContentHash string
}

func EnsureCoreSchema(db *sql.DB) error {
	if _, err := db.Exec(`
		PRAGMA foreign_keys = ON;

		CREATE TABLE IF NOT EXISTS documents (
			id INTEGER PRIMARY KEY,
			path TEXT NOT NULL UNIQUE,
			title TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS chunks (
			id INTEGER PRIMARY KEY,
			document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
			ordinal INTEGER NOT NULL,
			heading TEXT NOT NULL DEFAULT '',
			anchor TEXT NOT NULL DEFAULT '',
			text TEXT NOT NULL,
			content_hash TEXT NOT NULL DEFAULT '',
			UNIQUE(document_id, ordinal)
		);

		CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
			title,
			heading,
			text,
			tokenize="unicode61 tokenchars '` + aplTokenChars + `'"
		);
	`); err != nil {
		return fmt.Errorf("ensure semantic core schema: %w", err)
	}
	return nil
}

func EnsureVectorSchema(db *sql.DB, cfg VectorConfig) error {
	if _, err := db.Exec(VectorSchemaSQL(cfg)); err != nil {
		if strings.Contains(err.Error(), "no such module: vec0") {
			return fmt.Errorf("%w: sqlite-vec vec0 module is not loaded", ErrVectorExtensionUnavailable)
		}
		return fmt.Errorf("ensure vector schema: %w", err)
	}
	return nil
}

func VectorSchemaSQL(cfg VectorConfig) string {
	dimensions := cfg.Dimensions
	if dimensions == 0 {
		dimensions = DefaultEmbeddingDimensions
	}
	return fmt.Sprintf("CREATE VIRTUAL TABLE IF NOT EXISTS chunk_vec USING vec0(embedding FLOAT[%d]);", dimensions)
}

func VectorUpsertSQL() string {
	return "INSERT OR REPLACE INTO chunk_vec(rowid, embedding) VALUES (?, ?);"
}

func LoadVectorExtension(db *sql.DB, path string) error {
	if path == "" {
		return fmt.Errorf("%w: sqlite-vec extension path is empty", ErrVectorExtensionUnavailable)
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%w: sqlite-vec extension %q is not available: %v", ErrVectorExtensionUnavailable, path, err)
	}

	// SQLite extensions are connection-local. Until a ConnectHook-backed driver
	// is added, pin semantic vector work to one connection after loading sqlite-vec.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	conn, err := db.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("open sqlite connection for vector extension: %w", err)
	}
	defer conn.Close()

	if err := conn.Raw(func(driverConn any) error {
		sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("sqlite driver connection is %T, want *sqlite3.SQLiteConn", driverConn)
		}
		// sqlite-vec exports sqlite3_vec_init; mattn/go-sqlite3 would otherwise
		// derive sqlite3_vec0_init from the filename (vec0.dylib) and fail dlsym.
		return sqliteConn.LoadExtension(path, "sqlite3_vec_init")
	}); err != nil {
		return fmt.Errorf("%w: load sqlite-vec extension %q: %v", ErrVectorExtensionUnavailable, path, err)
	}
	return nil
}

func UpsertDocument(db *sql.DB, doc Document) (int64, error) {
	if doc.Path == "" {
		return 0, errors.New("document path is required")
	}
	if _, err := db.Exec(`
		INSERT INTO documents(path, title)
		VALUES (?, ?)
		ON CONFLICT(path) DO UPDATE SET title = excluded.title
	`, doc.Path, doc.Title); err != nil {
		return 0, fmt.Errorf("upsert document %q: %w", doc.Path, err)
	}

	var id int64
	if err := db.QueryRow(`SELECT id FROM documents WHERE path = ?`, doc.Path).Scan(&id); err != nil {
		return 0, fmt.Errorf("read document id %q: %w", doc.Path, err)
	}
	return id, nil
}

func UpsertChunk(db *sql.DB, chunk Chunk) (int64, error) {
	if chunk.DocumentID == 0 {
		return 0, errors.New("chunk document id is required")
	}
	if _, err := db.Exec(`
		INSERT INTO chunks(document_id, ordinal, heading, anchor, text, content_hash)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(document_id, ordinal) DO UPDATE SET
			heading = excluded.heading,
			anchor = excluded.anchor,
			text = excluded.text,
			content_hash = excluded.content_hash
	`, chunk.DocumentID, chunk.Ordinal, chunk.Heading, chunk.Anchor, chunk.Text, chunk.ContentHash); err != nil {
		return 0, fmt.Errorf("upsert chunk document_id=%d ordinal=%d: %w", chunk.DocumentID, chunk.Ordinal, err)
	}

	var id int64
	if err := db.QueryRow(`SELECT id FROM chunks WHERE document_id = ? AND ordinal = ?`, chunk.DocumentID, chunk.Ordinal).Scan(&id); err != nil {
		return 0, fmt.Errorf("read chunk id document_id=%d ordinal=%d: %w", chunk.DocumentID, chunk.Ordinal, err)
	}
	return id, nil
}

func DeleteDocument(db *sql.DB, path string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin delete document %q: %w", path, err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
		SELECT c.id
		FROM chunks c
		JOIN documents d ON d.id = c.document_id
		WHERE d.path = ?
	`, path)
	if err != nil {
		return fmt.Errorf("list chunks for deleted document %q: %w", path, err)
	}
	var chunkIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan deleted chunk id: %w", err)
		}
		chunkIDs = append(chunkIDs, id)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close deleted chunk rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate deleted chunk rows: %w", err)
	}

	for _, id := range chunkIDs {
		if _, err := tx.Exec(`DELETE FROM chunks_fts WHERE rowid = ?`, id); err != nil {
			return fmt.Errorf("delete FTS row for chunk %d: %w", id, err)
		}
	}
	hasVectorTable, err := tableExists(tx, "chunk_vec")
	if err != nil {
		return err
	}
	if hasVectorTable {
		for _, id := range chunkIDs {
			if _, err := tx.Exec(`DELETE FROM chunk_vec WHERE rowid = ?`, id); err != nil {
				return fmt.Errorf("delete vector row for chunk %d: %w", id, err)
			}
		}
	}
	for _, id := range chunkIDs {
		if _, err := tx.Exec(`DELETE FROM chunks WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete chunk %d: %w", id, err)
		}
	}

	if _, err := tx.Exec(`DELETE FROM documents WHERE path = ?`, path); err != nil {
		return fmt.Errorf("delete document %q: %w", path, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete document %q: %w", path, err)
	}
	return nil
}

func tableExists(tx *sql.Tx, name string) (bool, error) {
	var count int
	if err := tx.QueryRow(`
		SELECT COUNT(*)
		FROM sqlite_schema
		WHERE name = ?
		  AND type IN ('table', 'virtual table')
	`, name).Scan(&count); err != nil {
		return false, fmt.Errorf("check table %q exists: %w", name, err)
	}
	return count > 0, nil
}
