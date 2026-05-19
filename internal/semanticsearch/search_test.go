//go:build fts5

package semanticsearch

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/xpqz/bundle-docs/internal/semanticindex"
	"github.com/xpqz/bundle-docs/internal/semanticstore"
)

type queryEmbedder struct {
	vector []float32
}

func (e queryEmbedder) Embed(ctx context.Context, texts []string) (semanticindex.EmbeddingBatch, error) {
	embeddings := make([][]float32, len(texts))
	for i := range texts {
		embeddings[i] = append([]float32(nil), e.vector...)
	}
	return semanticindex.EmbeddingBatch{
		Model:      semanticindex.DefaultEmbeddingModel,
		Dimensions: len(e.vector),
		Embeddings: embeddings,
	}, nil
}

func TestQueryWeightsFavorFTSForExactAPLAndVectorForNaturalLanguage(t *testing.T) {
	for _, query := range []string{"⎕FIX", "⎕IO", ":If", `"namespace"`, "⍎", "∨", "∧", "/"} {
		exact := WeightsForQuery(query)
		if exact.FTS <= exact.Vector {
			t.Fatalf("weights for %q = %#v, want FTS > Vector", query, exact)
		}
	}
	natural := WeightsForQuery("how do I define a namespace")
	if natural.Vector <= natural.FTS {
		t.Fatalf("natural weights = %#v, want Vector > FTS", natural)
	}
}

func TestReciprocalRankFusionIsDeterministicAndExplainable(t *testing.T) {
	results := FuseResults([]RankedResult{
		{ChunkID: 10, Source: SourceFTS, Rank: 1},
		{ChunkID: 20, Source: SourceFTS, Rank: 2},
	}, []RankedResult{
		{ChunkID: 20, Source: SourceVector, Rank: 1},
		{ChunkID: 30, Source: SourceVector, Rank: 2},
	}, QueryWeights{FTS: 2.0, Vector: 1.0}, 10)

	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	if results[0].ChunkID != 20 {
		t.Fatalf("top result = %d, want fused chunk 20 under RRF: %#v", results[0].ChunkID, results)
	}
	if !strings.Contains(results[0].Explanation, "fts#2") || !strings.Contains(results[0].Explanation, "vector#1") {
		t.Fatalf("top explanation = %q, want fused ranks", results[0].Explanation)
	}
	if results[1].ChunkID != 10 || !strings.Contains(results[1].Explanation, "fts#1") {
		t.Fatalf("second result = %#v, want FTS-only chunk 10", results[1])
	}
}

func TestVectorSearchSQLUsesSqliteVecKNNShape(t *testing.T) {
	sql := VectorSearchSQL()
	for _, want := range []string{"chunk_vec", "embedding MATCH", "k = ?", "distance"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("VectorSearchSQL missing %q:\n%s", want, sql)
		}
	}
}

func TestSearchChunksSupportsFTSVectorAndHybridModes(t *testing.T) {
	db := openSearchDB(t)
	insertSearchChunk(t, db, "Language / Fix", "Fix Script", "⎕FIX", "⎕FIX fixes a script.", []float32{1, 0, 0})
	insertSearchChunk(t, db, "Language / Namespaces", "Namespaces", "Namespace references", "Namespace references resolve names.", []float32{0, 1, 0})

	fts, err := SearchChunks(context.Background(), db, nil, SearchOptions{
		Query: "⎕FIX",
		Mode:  ModeFTS,
		Limit: 5,
	})
	if err != nil {
		t.Fatalf("FTS search: %v", err)
	}
	if len(fts) != 1 || fts[0].Heading != "⎕FIX" || fts[0].Source != SourceFTS {
		t.Fatalf("FTS results = %#v", fts)
	}

	vector, err := SearchChunks(context.Background(), db, queryEmbedder{vector: []float32{0, 1, 0}}, SearchOptions{
		Query:               "how do I define a namespace",
		Mode:                ModeVector,
		Limit:               5,
		VectorDims:          3,
		UseFallbackVectorDB: true,
	})
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(vector) == 0 || vector[0].Heading != "Namespace references" || vector[0].Source != SourceVector {
		t.Fatalf("vector results = %#v", vector)
	}

	hybrid, err := SearchChunks(context.Background(), db, queryEmbedder{vector: []float32{0, 1, 0}}, SearchOptions{
		Query:               "namespace reference evaluation",
		Mode:                ModeHybrid,
		Limit:               5,
		VectorDims:          3,
		UseFallbackVectorDB: true,
	})
	if err != nil {
		t.Fatalf("hybrid search: %v", err)
	}
	if len(hybrid) == 0 || hybrid[0].Path == "" || hybrid[0].Snippet == "" || hybrid[0].Explanation == "" {
		t.Fatalf("hybrid results missing rendering context: %#v", hybrid)
	}
}

func TestFormatResultsIncludesDocumentContextAndCompactSnippet(t *testing.T) {
	output := FormatResults([]SearchResult{{
		ChunkID:     42,
		Title:       "Namespaces",
		Path:        "Language / Namespaces",
		Heading:     "Namespace references",
		Snippet:     "Namespace references resolve names at evaluation time in the current namespace.",
		Score:       0.42,
		Source:      SourceHybrid,
		Explanation: "fts#1 + vector#2",
	}})

	for _, want := range []string{"42", "Namespaces", "Language / Namespaces", "Namespace references", "fts#1 + vector#2"} {
		if !strings.Contains(output, want) {
			t.Fatalf("formatted output missing %q:\n%s", want, output)
		}
	}
	if len(output) > 220 {
		t.Fatalf("formatted output is too verbose (%d bytes):\n%s", len(output), output)
	}
}

func openSearchDB(t *testing.T) *sql.DB {
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
		t.Fatalf("ensure core schema: %v", err)
	}
	execSearchSQL(t, db, `CREATE TABLE chunk_vec(rowid INTEGER PRIMARY KEY, embedding TEXT NOT NULL)`)
	return db
}

func insertSearchChunk(t *testing.T, db *sql.DB, path, title, heading, text string, vector []float32) int64 {
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
	execSearchSQL(t, db, `
		INSERT INTO chunks_fts(rowid, title, heading, text)
		VALUES (?, ?, ?, ?)
	`, chunkID, title, heading, text)
	execSearchSQL(t, db, semanticstore.VectorUpsertSQL(), chunkID, encodeVectorForTest(vector))
	return chunkID
}

func execSearchSQL(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()

	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
}

func encodeVectorForTest(vector []float32) string {
	parts := make([]string, len(vector))
	for i, v := range vector {
		parts[i] = strconv.FormatFloat(float64(v), 'f', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
