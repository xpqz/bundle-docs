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

func TestFTSMatchExpressionUsesPhraseForExactAndORForNaturalLanguage(t *testing.T) {
	cases := []struct {
		name, query, want string
	}{
		{"glyph stays a phrase", "⎕FIX", `"⎕FIX"`},
		{"colon prefix stays a phrase", ":If", `":If"`},
		{"quoted query stays a phrase", `"namespace"`, `"""namespace"""`},
		{"natural language OR-tokens", "find where an array equals a value", `"find" OR "where" OR "array" OR "equals" OR "value"`},
		{"common stopwords dropped", "how do I define a namespace", `"define" OR "namespace"`},
		{"single significant token stays a phrase", "namespace", `"namespace"`},
		{"duplicates collapsed", "format format numbers", `"format" OR "numbers"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ftsMatchExpression(tc.query); got != tc.want {
				t.Fatalf("ftsMatchExpression(%q) = %q, want %q", tc.query, got, tc.want)
			}
		})
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

func TestIsCanonicalChunkTrueWhenHeadingMatchesOrIsContainedInTitle(t *testing.T) {
	cases := []struct {
		name    string
		title   string
		heading string
		want    bool
	}{
		{"equal", "Execute R←⍎Y", "Execute R←⍎Y", true},
		{"heading is title prefix", "Fix Script {R}←{X}⎕FIX Y", "Fix Script", true},
		{"heading inside title", "ExecuteJavaScript Method 839", "Method 839", true},
		{"sub-section heading", "Execute R←⍎Y", "Examples", false},
		{"warning sub-section", "Where R←⍸Y", "Restriction", false},
		{"empty title", "", "Whatever", false},
		{"empty heading", "Title", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCanonicalChunk(tc.title, tc.heading); got != tc.want {
				t.Fatalf("isCanonicalChunk(%q, %q) = %v, want %v", tc.title, tc.heading, got, tc.want)
			}
		})
	}
}

func TestHybridSearchPromotesCanonicalChunkOverKeywordHeavySubsection(t *testing.T) {
	// Reproduces the "execute character vector as code" shape: a
	// long sub-section chunk wins on vector rank, while the
	// canonical reference chunk for the relevant primitive sits a
	// few positions lower. With deep candidate fetch + canonical
	// bonus, the canonical chunk should land at top-1.
	db := openSearchDB(t)

	// Canonical Execute reference chunk - heading equals title, terse body.
	execID := insertSearchChunk(t, db, "Core Reference / Execute", "Execute R←⍎Y", "Execute R←⍎Y",
		"Warning: untrusted input is risky.", []float32{0.2, 0.9, 0.1})
	// Long, keyword-rich sub-section on a different page.
	_ = insertSearchChunk(t, db, "GUI / Character", "Character Input/Output ⍞", "Examples",
		"Character vector input and output examples with code samples.", []float32{0.1, 1.0, 0.0})

	results, err := SearchChunks(context.Background(), db, queryEmbedder{vector: []float32{0.1, 1.0, 0.0}}, SearchOptions{
		Query:               "execute character vector as code",
		Mode:                ModeHybrid,
		Limit:               5,
		VectorDims:          3,
		UseFallbackVectorDB: true,
	})
	if err != nil {
		t.Fatalf("hybrid search: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("no hybrid results")
	}
	if results[0].ChunkID != execID {
		t.Fatalf("hybrid top-1 = %d (%q), want canonical Execute chunk %d",
			results[0].ChunkID, results[0].Heading, execID)
	}
	if !strings.Contains(results[0].Explanation, "canonical") {
		t.Fatalf("top result explanation = %q, want canonical marker", results[0].Explanation)
	}
}

func TestDedupeByDocumentKeepsFirstChunkPerPath(t *testing.T) {
	in := []SearchResult{
		{ChunkID: 1, Path: "A / X", Score: 0.9},
		{ChunkID: 2, Path: "A / X", Score: 0.7}, // dupe of #1
		{ChunkID: 3, Path: "B / Y", Score: 0.5},
		{ChunkID: 4, Path: "", Score: 0.4},     // no path - keep
		{ChunkID: 5, Path: "A / X", Score: 0.3}, // dupe of #1
	}
	got := dedupeByDocument(in)
	wantIDs := []int64{1, 3, 4}
	if len(got) != len(wantIDs) {
		t.Fatalf("len = %d, want %d: %#v", len(got), len(wantIDs), got)
	}
	for i, id := range wantIDs {
		if got[i].ChunkID != id {
			t.Fatalf("got[%d].ChunkID = %d, want %d", i, got[i].ChunkID, id)
		}
	}
}

func TestSearchChunksDedupesByDocumentWhenRequested(t *testing.T) {
	db := openSearchDB(t)
	// Two chunks from the same document (same path), one from a
	// different document. With DedupeByDocument the second chunk of
	// the dupe page should be dropped.
	insertSearchChunkAt(t, db, 0, "Lang / Execute", "Execute R←⍎Y", "Execute R←⍎Y",
		"executing character vectors", []float32{0, 1, 0})
	insertSearchChunkAt(t, db, 1, "Lang / Execute", "Execute R←⍎Y", "Examples",
		"⍎'2+2' yields 4", []float32{0.1, 0.95, 0})
	insertSearchChunkAt(t, db, 0, "Lang / Namespaces", "Namespaces", "Namespaces",
		"namespaces resolve names", []float32{0.1, 0.1, 0.9})

	options := SearchOptions{
		Query:               "execute character vector",
		Mode:                ModeVector,
		Limit:               5,
		VectorDims:          3,
		UseFallbackVectorDB: true,
		DedupeByDocument:    true,
	}
	results, err := SearchChunks(context.Background(), db, queryEmbedder{vector: []float32{0, 1, 0}}, options)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 after dedup: %#v", len(results), results)
	}
	seen := map[string]bool{}
	for _, r := range results {
		if seen[r.Path] {
			t.Fatalf("dedup failed, %q appears twice: %#v", r.Path, results)
		}
		seen[r.Path] = true
	}
}

func TestCompactSnippetStripsMarkdownNoise(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "fenced apl code keeps content, drops backticks and lang",
			in:   "```apl\na←'ab' b←1(220⌶)a\nb ¯33 ¯108 5 0 0 0\n```",
			want: "a←'ab' b←1(220⌶)a b ¯33 ¯108 5 0 0 0",
		},
		{
			name: "inline code backticks removed",
			in:   "Use `⎕FIX` to load.",
			want: "Use ⎕FIX to load.",
		},
		{
			name: "markdown link collapses to anchor text",
			in:   "See [`⎕VGET`](../system-functions/vget.md) for details.",
			want: "See ⎕VGET for details.",
		},
		{
			name: "admonition opener stripped",
			in:   "!!! Warning \"Warning\"\nIf the argument to _execute_ could include user input...",
			want: "If the argument to execute could include user input...",
		},
		{
			name: "heading hash stripped",
			in:   "## Examples\nfoo",
			want: "Examples foo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := compactSnippet(tc.in, 200); got != tc.want {
				t.Fatalf("compactSnippet:\n got: %q\nwant: %q", got, tc.want)
			}
		})
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

func insertSearchChunkAt(t *testing.T, db *sql.DB, ordinal int, path, title, heading, text string, vector []float32) int64 {
	t.Helper()

	docID, err := semanticstore.UpsertDocument(db, semanticstore.Document{Path: path, Title: title})
	if err != nil {
		t.Fatalf("upsert document: %v", err)
	}
	chunkID, err := semanticstore.UpsertChunk(db, semanticstore.Chunk{
		DocumentID:  docID,
		Ordinal:     ordinal,
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
