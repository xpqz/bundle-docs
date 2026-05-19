//go:build semantic

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

func TestStripHTMLTagsCleansBreadcrumb(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Core Reference / <code>⎕FIX</code>: Fix Script", "Core Reference / ⎕FIX: Fix Script"},
		{"plain text", "plain text"},
		{"<b>bold</b> and <em>em</em>", "bold and em"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := stripHTMLTags(tc.in); got != tc.want {
			t.Fatalf("stripHTMLTags(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestApplyBonusesBoostsLanguageReferenceForExactQueries(t *testing.T) {
	// Scores chosen to mirror real FTS rank #1 and #2 (1/61 and 1/62).
	results := []SearchResult{
		{ChunkID: 1, Path: "Release Notes / Earlier / v19 / Fix Script", Score: 1.0 / 61.0, Explanation: "fts#1"},
		{ChunkID: 2, Path: "Core Reference / Dyalog APL Language / System Functions / Fix Script", Score: 1.0 / 62.0, Explanation: "fts#2"},
	}
	applyBonuses(results, "⎕FIX")

	if results[0].Score >= results[1].Score {
		t.Fatalf("exact APL query: Core Reference chunk should outscore Release Notes after bonus.\n  RN=%v  Core=%v", results[0], results[1])
	}
	if !strings.Contains(results[1].Explanation, "ref") {
		t.Fatalf("Core Reference chunk should be tagged 'ref': %q", results[1].Explanation)
	}
	if strings.Contains(results[0].Explanation, "ref") {
		t.Fatalf("Release Notes chunk should not receive 'ref' tag: %q", results[0].Explanation)
	}
}

func TestApplyBonusesForcesTitleMatchToTopForExactQueries(t *testing.T) {
	// Reproduces the ⎕IO failure: ⎕STATE is also under Core Reference
	// and ranks high in both FTS and vector for ⎕IO queries, so it
	// can outscore the canonical ⎕IO chunk without an explicit title-
	// match override.
	results := []SearchResult{
		{ChunkID: 1, Title: "State of Object R←⎕STATE Y", Heading: "Example",
			Path: "Core Reference / Dyalog APL Language / System Functions / ⎕STATE: State of Object",
			Score: 0.04},
		{ChunkID: 2, Title: "Index Origin ⎕IO", Heading: "Index Origin ⎕IO",
			Path: "Core Reference / Dyalog APL Language / System Functions / ⎕IO: Index Origin",
			Score: 0.03},
	}
	applyBonuses(results, "⎕IO")

	if results[1].Score <= results[0].Score {
		t.Fatalf("title-match should win.\n  STATE=%v  IO=%v", results[0], results[1])
	}
	if !strings.Contains(results[1].Explanation, "title") {
		t.Fatalf("⎕IO chunk should be tagged 'title': %q", results[1].Explanation)
	}
	if strings.Contains(results[0].Explanation, "title") {
		t.Fatalf("⎕STATE chunk must not get title bonus for ⎕IO query: %q", results[0].Explanation)
	}
}

func TestApplyBonusesFallsBackToHeadingWhenTitleIsWrong(t *testing.T) {
	// Real-world case: bundle-docs extracted a bogus title for the
	// ⎕OR page ("'ORTEST' ⎕FCREATE 1") but the chunker captured the
	// correct H1 as the chunk heading. Title-match must still fire so
	// the canonical ⎕OR chunk surfaces.
	results := []SearchResult{
		{ChunkID: 1, Title: "'ORTEST' ⎕FCREATE 1", Heading: "Object Representation R←⎕OR Y",
			Path: "Core Reference / Dyalog APL Language / System Functions / ⎕OR: Object Representation",
			Score: 0.01},
		{ChunkID: 2, Title: "Something Else", Heading: "Other",
			Path: "Core Reference / Dyalog APL Language / Misc",
			Score: 0.05},
	}
	applyBonuses(results, "⎕OR")
	if results[0].Score <= results[1].Score {
		t.Fatalf("heading-match should win.\n  ⎕OR=%v  Other=%v", results[0], results[1])
	}
	if !strings.Contains(results[0].Explanation, "title") {
		t.Fatalf("⎕OR chunk should be tagged 'title' (matched via heading): %q", results[0].Explanation)
	}
}

func TestApplyBonusesTitleMatchHandlesQuotedQueries(t *testing.T) {
	results := []SearchResult{
		{ChunkID: 1, Title: "Namespaces", Heading: "Overview"},
		{ChunkID: 2, Title: "Other", Heading: "Other"},
	}
	applyBonuses(results, `"namespace"`)
	if !strings.Contains(results[0].Explanation, "title") {
		t.Fatalf("quoted query should still title-match: %q", results[0].Explanation)
	}
}

func TestApplyBonusesSkipsTitleMatchForNaturalQueries(t *testing.T) {
	results := []SearchResult{
		{ChunkID: 1, Title: "Format Date-time", Heading: "Examples"},
	}
	applyBonuses(results, "format numbers as text")
	if strings.Contains(results[0].Explanation, "title") {
		t.Fatalf("natural-language query must not trigger title-match: %q", results[0].Explanation)
	}
}

func TestApplyBonusesSkipsLanguageReferenceForNaturalQueries(t *testing.T) {
	results := []SearchResult{
		{ChunkID: 1, Path: "Release Notes / Earlier / v19", Score: 0.10, Explanation: "fts#1"},
		{ChunkID: 2, Path: "Core Reference / Dyalog APL Language / X", Score: 0.10, Explanation: "fts#2"},
	}
	applyBonuses(results, "how do I trap errors")
	for _, r := range results {
		if strings.Contains(r.Explanation, "ref") {
			t.Fatalf("natural-language query should not get ref bonus: %#v", r)
		}
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
