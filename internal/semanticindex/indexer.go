//go:build semantic

package semanticindex

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/xpqz/bundle-docs/internal/semanticstore"
)

type IndexOptions struct {
	Chunk               ChunkOptions
	BatchSize           int
	VectorDims          int
	UseFallbackVectorDB bool
}

type IndexStats struct {
	Documents  int
	Chunks     int
	Embeddings int
}

func IndexDatabase(ctx context.Context, db *sql.DB, embedder Embedder, options IndexOptions) (IndexStats, error) {
	if embedder == nil {
		return IndexStats{}, fmt.Errorf("embedder is required")
	}
	if options.BatchSize <= 0 {
		options.BatchSize = 32
	}
	if options.VectorDims <= 0 {
		options.VectorDims = semanticstore.DefaultEmbeddingDimensions
	}
	if options.Chunk.MaxTokens <= 0 {
		options.Chunk = DefaultChunkOptions()
	}

	if err := semanticstore.EnsureCoreSchema(db); err != nil {
		return IndexStats{}, err
	}
	if options.UseFallbackVectorDB {
		if err := ensureFallbackVectorTable(db); err != nil {
			return IndexStats{}, err
		}
	} else if err := semanticstore.EnsureVectorSchema(db, semanticstore.VectorConfig{Dimensions: options.VectorDims}); err != nil {
		return IndexStats{}, err
	}

	sourceDocs, err := readSourceDocuments(ctx, db)
	if err != nil {
		return IndexStats{}, err
	}

	seenDocuments := make(map[string]bool)
	stats := IndexStats{}
	for _, doc := range sourceDocs {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		seenDocuments[doc.Path] = true
		stats.Documents++

		documentID, err := semanticstore.UpsertDocument(db, semanticstore.Document{
			Path:  doc.Path,
			Title: doc.Title,
		})
		if err != nil {
			return stats, err
		}

		chunks := ChunkMarkdown(doc, options.Chunk)
		if err := removeStaleChunks(db, documentID, len(chunks)); err != nil {
			return stats, err
		}
		for _, chunk := range chunks {
			chunkID, err := semanticstore.UpsertChunk(db, semanticstore.Chunk{
				DocumentID:  documentID,
				Ordinal:     chunk.Ordinal,
				Heading:     chunk.Heading,
				Anchor:      chunk.Anchor,
				Text:        chunk.Text,
				ContentHash: chunk.ContentHash,
			})
			if err != nil {
				return stats, err
			}
			if err := upsertFTS(db, chunkID, doc.Title, chunk.Heading, chunk.Text); err != nil {
				return stats, err
			}
			chunks[chunk.Ordinal] = chunk
			stats.Chunks++
		}
		if err := embedAndStore(ctx, db, embedder, chunks, documentID, options); err != nil {
			return stats, err
		}
		stats.Embeddings += len(chunks)
	}

	if err := removeDeletedDocuments(db, seenDocuments); err != nil {
		return stats, err
	}
	return stats, nil
}

func readSourceDocuments(ctx context.Context, db *sql.DB) ([]SourceDocument, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT path, file, title, content
		FROM docs
		WHERE exclude = 0
		ORDER BY path
	`)
	if err != nil {
		return nil, fmt.Errorf("read source docs: %w", err)
	}
	defer rows.Close()

	var docs []SourceDocument
	for rows.Next() {
		var doc SourceDocument
		if err := rows.Scan(&doc.Path, &doc.File, &doc.Title, &doc.Content); err != nil {
			return nil, fmt.Errorf("scan source doc: %w", err)
		}
		docs = append(docs, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate source docs: %w", err)
	}
	return docs, nil
}

func ensureFallbackVectorTable(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS chunk_vec(rowid INTEGER PRIMARY KEY, embedding TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("ensure fallback vector table: %w", err)
	}
	return nil
}

func upsertFTS(db *sql.DB, chunkID int64, title, heading, text string) error {
	if _, err := db.Exec(`DELETE FROM chunks_fts WHERE rowid = ?`, chunkID); err != nil {
		return fmt.Errorf("delete old FTS row %d: %w", chunkID, err)
	}
	if _, err := db.Exec(`INSERT INTO chunks_fts(rowid, title, heading, text) VALUES (?, ?, ?, ?)`, chunkID, title, heading, text); err != nil {
		return fmt.Errorf("insert FTS row %d: %w", chunkID, err)
	}
	return nil
}

func embedAndStore(ctx context.Context, db *sql.DB, embedder Embedder, chunks []MarkdownChunk, documentID int64, options IndexOptions) error {
	for start := 0; start < len(chunks); start += options.BatchSize {
		end := start + options.BatchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		batchChunks := chunks[start:end]
		texts := make([]string, len(batchChunks))
		for i, chunk := range batchChunks {
			texts[i] = EmbeddingText(chunk)
		}
		batch, err := embedder.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("embed chunks for document_id=%d batch=%d: %w", documentID, start/options.BatchSize, err)
		}
		if len(batch.Embeddings) != len(batchChunks) {
			return fmt.Errorf("embedding count %d does not match chunk count %d", len(batch.Embeddings), len(batchChunks))
		}
		for i, vector := range batch.Embeddings {
			if len(vector) != options.VectorDims || batch.Dimensions != options.VectorDims {
				return fmt.Errorf("embedding dimensions = vector:%d response:%d, want %d", len(vector), batch.Dimensions, options.VectorDims)
			}
			chunkID, err := lookupChunkID(db, documentID, batchChunks[i].Ordinal)
			if err != nil {
				return err
			}
			if err := semanticstore.UpsertChunkVector(db, chunkID, encodeVectorJSON(vector)); err != nil {
				return fmt.Errorf("store vector for chunk %d: %w", chunkID, err)
			}
		}
	}
	return nil
}

func lookupChunkID(db *sql.DB, documentID int64, ordinal int) (int64, error) {
	var id int64
	if err := db.QueryRow(`SELECT id FROM chunks WHERE document_id = ? AND ordinal = ?`, documentID, ordinal).Scan(&id); err != nil {
		return 0, fmt.Errorf("lookup chunk document_id=%d ordinal=%d: %w", documentID, ordinal, err)
	}
	return id, nil
}

func removeStaleChunks(db *sql.DB, documentID int64, keepCount int) error {
	rows, err := db.Query(`SELECT id FROM chunks WHERE document_id = ? AND ordinal >= ?`, documentID, keepCount)
	if err != nil {
		return fmt.Errorf("list stale chunks document_id=%d: %w", documentID, err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan stale chunk id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close stale chunk rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate stale chunks: %w", err)
	}
	for _, id := range ids {
		if _, err := db.Exec(`DELETE FROM chunks_fts WHERE rowid = ?`, id); err != nil {
			return fmt.Errorf("delete stale FTS row %d: %w", id, err)
		}
		if _, err := db.Exec(`DELETE FROM chunk_vec WHERE rowid = ?`, id); err != nil {
			return fmt.Errorf("delete stale vector row %d: %w", id, err)
		}
		if _, err := db.Exec(`DELETE FROM chunks WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete stale chunk %d: %w", id, err)
		}
	}
	return nil
}

func removeDeletedDocuments(db *sql.DB, seen map[string]bool) error {
	rows, err := db.Query(`SELECT path FROM documents ORDER BY path`)
	if err != nil {
		return fmt.Errorf("list semantic documents: %w", err)
	}
	var stale []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return fmt.Errorf("scan semantic document path: %w", err)
		}
		if !seen[path] {
			stale = append(stale, path)
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close semantic document rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate semantic documents: %w", err)
	}
	for _, path := range stale {
		if err := semanticstore.DeleteDocument(db, path); err != nil {
			return err
		}
	}
	return nil
}
