//go:build fts5

package semanticsearch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/xpqz/bundle-docs/internal/semanticindex"
)

type Mode string

const (
	ModeFTS    Mode = "fts"
	ModeVector Mode = "vector"
	ModeHybrid Mode = "hybrid"
)

type Source string

const (
	SourceFTS    Source = "fts"
	SourceVector Source = "vector"
	SourceHybrid Source = "hybrid"
)

type QueryWeights struct {
	FTS    float64
	Vector float64
}

type SearchOptions struct {
	Query               string
	Mode                Mode
	Limit               int
	VectorDims          int
	UseFallbackVectorDB bool
	// DedupeByDocument keeps only the highest-scoring chunk per
	// documents.path. Useful for UIs that want one row per page;
	// the eval workflow wants every chunk so this is opt-in.
	DedupeByDocument bool
}

type RankedResult struct {
	ChunkID     int64
	Source      Source
	Rank        int
	Score       float64
	Explanation string
}

type SearchResult struct {
	ChunkID     int64
	Title       string
	Path        string
	Heading     string
	Snippet     string
	Score       float64
	Source      Source
	Explanation string
}

func WeightsForQuery(query string) QueryWeights {
	if looksExact(query) {
		return QueryWeights{FTS: 2.0, Vector: 0.5}
	}
	return QueryWeights{FTS: 0.8, Vector: 1.8}
}

func looksExact(query string) bool {
	query = strings.TrimSpace(query)
	if strings.HasPrefix(query, `"`) && strings.HasSuffix(query, `"`) {
		return true
	}
	if strings.HasPrefix(query, ":") {
		return true
	}
	return ContainsAPL(query)
}

func FuseResults(fts, vector []RankedResult, weights QueryWeights, limit int) []SearchResult {
	if limit <= 0 {
		limit = 10
	}
	const offset = 60.0
	type accum struct {
		id           int64
		score        float64
		explanations []string
		hasFTS       bool
		hasVector    bool
	}
	byID := make(map[int64]*accum)
	add := func(result RankedResult, weight float64) {
		if result.Rank <= 0 {
			return
		}
		item := byID[result.ChunkID]
		if item == nil {
			item = &accum{id: result.ChunkID}
			byID[result.ChunkID] = item
		}
		item.score += weight / (offset + float64(result.Rank))
		item.explanations = append(item.explanations, fmt.Sprintf("%s#%d", result.Source, result.Rank))
		if result.Source == SourceFTS {
			item.hasFTS = true
		}
		if result.Source == SourceVector {
			item.hasVector = true
		}
	}
	for _, result := range fts {
		add(result, weights.FTS)
	}
	for _, result := range vector {
		add(result, weights.Vector)
	}

	results := make([]SearchResult, 0, len(byID))
	for _, item := range byID {
		source := SourceHybrid
		if item.hasFTS && !item.hasVector {
			source = SourceFTS
		} else if item.hasVector && !item.hasFTS {
			source = SourceVector
		}
		results = append(results, SearchResult{
			ChunkID:     item.id,
			Score:       item.score,
			Source:      source,
			Explanation: strings.Join(item.explanations, " + "),
		})
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].ChunkID < results[j].ChunkID
		}
		return results[i].Score > results[j].Score
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results
}

func SearchChunks(ctx context.Context, db *sql.DB, embedder semanticindex.Embedder, options SearchOptions) ([]SearchResult, error) {
	if options.Limit <= 0 {
		options.Limit = 10
	}
	if options.VectorDims <= 0 {
		options.VectorDims = 384
	}
	switch options.Mode {
	case "", ModeHybrid:
		options.Mode = ModeHybrid
	case ModeFTS, ModeVector:
	default:
		return nil, fmt.Errorf("unknown semantic mode %q", options.Mode)
	}

	// Fetch deeper per-side pools when we need RRF to see lower-ranked
	// canonical matches (hybrid on natural-language queries) or when
	// we are about to collapse chunks to one-per-document (dedupe
	// often discards 4-6 of the top results). 50 is a soft cap to
	// keep latency bounded. Exact-looking APL queries in hybrid mode
	// keep the tight limit because FTS is authoritative there and
	// pulling deeper vector candidates can let a tangentially-related
	// page (e.g. a ⎕STATE doc that mentions ⎕IO) outscore fts#1.
	perSideLimit := options.Limit
	if options.DedupeByDocument || (options.Mode == ModeHybrid && !looksExact(options.Query)) {
		perSideLimit = options.Limit * 4
		if perSideLimit > 50 {
			perSideLimit = 50
		}
	}

	var fts []RankedResult
	var vector []RankedResult
	var err error
	if options.Mode == ModeFTS || options.Mode == ModeHybrid {
		fts, err = searchFTS(ctx, db, options.Query, perSideLimit)
		if err != nil {
			return nil, err
		}
	}
	if options.Mode == ModeVector || options.Mode == ModeHybrid {
		if embedder == nil {
			return nil, fmt.Errorf("embedding service is required for %s mode", options.Mode)
		}
		vectorOptions := options
		vectorOptions.Limit = perSideLimit
		vector, err = searchVector(ctx, db, embedder, vectorOptions)
		if err != nil {
			return nil, err
		}
	}

	var fused []SearchResult
	switch options.Mode {
	case ModeFTS:
		fused = rankedToResults(fts, SourceFTS, perSideLimit)
	case ModeVector:
		fused = rankedToResults(vector, SourceVector, perSideLimit)
	default:
		// Fuse on the deep pools so the canonical chunks can compete.
		fused = FuseResults(fts, vector, WeightsForQuery(options.Query), len(fts)+len(vector))
	}
	if err := hydrateResults(ctx, db, fused); err != nil {
		return nil, err
	}
	// applyBonuses is universal now: the Language Reference boost
	// for exact APL queries needs to fire in fts-only and vector-only
	// modes too, otherwise verbatim ⎕FIX still surfaces the release
	// notes chunk above the canonical reference page.
	applyBonuses(fused, options.Query)
	sort.Slice(fused, func(i, j int) bool {
		if fused[i].Score == fused[j].Score {
			return fused[i].ChunkID < fused[j].ChunkID
		}
		return fused[i].Score > fused[j].Score
	})
	if options.DedupeByDocument {
		fused = dedupeByDocument(fused)
	}
	if len(fused) > options.Limit {
		fused = fused[:options.Limit]
	}
	return fused, nil
}

// dedupeByDocument keeps the first occurrence of each documents.path
// (the navigation breadcrumb), which after sort is the highest-scoring
// chunk for that document. Order is preserved otherwise.
func dedupeByDocument(results []SearchResult) []SearchResult {
	seen := make(map[string]struct{}, len(results))
	out := make([]SearchResult, 0, len(results))
	for _, r := range results {
		if r.Path == "" {
			out = append(out, r)
			continue
		}
		if _, dup := seen[r.Path]; dup {
			continue
		}
		seen[r.Path] = struct{}{}
		out = append(out, r)
	}
	return out
}

// canonicalChunkBonus is added to the fused score of chunks whose
// heading is the document's canonical heading (heading equals the
// title, or the title contains the heading). Those chunks are the
// reference body of a primitive page rather than an Examples or
// Warning sub-section. The magnitude is tuned as a tiebreaker - large
// enough to break ties between an FTS#1 hit and an FTS#2 hit on the
// same page, small enough that a clear vector#1 result still wins.
const canonicalChunkBonus = 0.003

// languageReferenceBonus is added to chunks under the Language
// Reference Guide breadcrumb when the query looks like an exact APL
// glyph, system function, or control word. It ensures a verbatim
// query like "⎕FIX" surfaces the canonical reference page ahead of
// release-notes coverage of the same symbol.
const languageReferenceBonus = 0.005

const languageReferencePrefix = "Core Reference / Dyalog APL Language /"

// applyBonuses adjusts the fused scores after hydration. It handles
// both the canonical-chunk tiebreaker and the Language Reference
// boost for exact queries. The Explanation field is annotated so
// the UI shows which bonuses fired.
func applyBonuses(results []SearchResult, query string) {
	exact := looksExact(query)
	for i := range results {
		var tags []string
		if isCanonicalChunk(results[i].Title, results[i].Heading) {
			results[i].Score += canonicalChunkBonus
			tags = append(tags, "canonical")
		}
		if exact && strings.HasPrefix(results[i].Path, languageReferencePrefix) {
			results[i].Score += languageReferenceBonus
			tags = append(tags, "ref")
		}
		if len(tags) == 0 {
			continue
		}
		extra := strings.Join(tags, " + ")
		if results[i].Explanation == "" {
			results[i].Explanation = extra
		} else {
			results[i].Explanation += " + " + extra
		}
	}
}

func isCanonicalChunk(title, heading string) bool {
	title = strings.TrimSpace(title)
	heading = strings.TrimSpace(heading)
	if title == "" || heading == "" {
		return false
	}
	if title == heading {
		return true
	}
	return strings.Contains(title, heading)
}

func searchFTS(ctx context.Context, db *sql.DB, query string, limit int) ([]RankedResult, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT rowid
		FROM chunks_fts
		WHERE chunks_fts MATCH ?
		ORDER BY bm25(chunks_fts), rowid
		LIMIT ?
	`, ftsMatchExpression(query), limit)
	if err != nil {
		return nil, fmt.Errorf("semantic FTS search: %w", err)
	}
	defer rows.Close()
	return scanRanked(rows, SourceFTS)
}

// ftsMatchExpression builds an FTS5 MATCH expression from the user query.
//
// Exact-looking queries (APL glyphs, colon prefixes, quoted phrases) keep
// phrase matching so that "⎕FIX", ":If", and "⍳" still hit precisely.
// Natural-language queries are split into significant tokens and OR'd so
// that a doc whose title contains one of the tokens (e.g. "Execute" or
// "Where" in a primitive function page) can surface even when the full
// phrase does not appear anywhere in the corpus.
func ftsMatchExpression(query string) string {
	query = strings.TrimSpace(query)
	if looksExact(query) {
		return quoteFTS(query)
	}
	tokens := significantTokens(query)
	if len(tokens) <= 1 {
		return quoteFTS(query)
	}
	parts := make([]string, len(tokens))
	for i, tok := range tokens {
		parts[i] = quoteFTS(tok)
	}
	return strings.Join(parts, " OR ")
}

// ftsStopwords excludes common English question/connective words that
// would otherwise dominate bm25 scoring without adding retrieval signal.
var ftsStopwords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "as": {}, "at": {}, "be": {},
	"by": {}, "do": {}, "for": {}, "from": {}, "how": {}, "i": {}, "in": {},
	"is": {}, "it": {}, "of": {}, "on": {}, "or": {}, "the": {}, "this": {},
	"to": {}, "what": {}, "which": {}, "with": {}, "you": {},
}

func significantTokens(query string) []string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("⍵⍺⎕⍳⍸⍎", r)
	})
	seen := make(map[string]struct{}, len(fields))
	out := make([]string, 0, len(fields))
	for _, tok := range fields {
		lower := strings.ToLower(tok)
		if len(lower) < 2 {
			continue
		}
		if _, stop := ftsStopwords[lower]; stop {
			continue
		}
		if _, dup := seen[lower]; dup {
			continue
		}
		seen[lower] = struct{}{}
		out = append(out, tok)
	}
	return out
}

func searchVector(ctx context.Context, db *sql.DB, embedder semanticindex.Embedder, options SearchOptions) ([]RankedResult, error) {
	batch, err := embedder.Embed(ctx, []string{options.Query})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(batch.Embeddings) != 1 {
		return nil, fmt.Errorf("query embedding count = %d, want 1", len(batch.Embeddings))
	}
	if batch.Dimensions != options.VectorDims || len(batch.Embeddings[0]) != options.VectorDims {
		return nil, fmt.Errorf("query embedding dimensions = response:%d vector:%d, want %d", batch.Dimensions, len(batch.Embeddings[0]), options.VectorDims)
	}
	vectorJSON := encodeVector(batch.Embeddings[0])
	if options.UseFallbackVectorDB {
		return searchVectorFallback(ctx, db, batch.Embeddings[0], options.Limit)
	}
	rows, err := db.QueryContext(ctx, VectorSearchSQL(), vectorJSON, options.Limit)
	if err != nil {
		return nil, fmt.Errorf("semantic vector search: %w", err)
	}
	defer rows.Close()
	results, err := scanRankedWithDistance(rows, SourceVector)
	if err != nil {
		return nil, err
	}
	return results, nil
}

func VectorSearchSQL() string {
	return `
		SELECT rowid, distance
		FROM chunk_vec
		WHERE embedding MATCH ? AND k = ?
		ORDER BY distance
	`
}

func searchVectorFallback(ctx context.Context, db *sql.DB, query []float32, limit int) ([]RankedResult, error) {
	rows, err := db.QueryContext(ctx, `SELECT rowid, embedding FROM chunk_vec`)
	if err != nil {
		return nil, fmt.Errorf("semantic vector fallback search: %w", err)
	}
	defer rows.Close()
	type item struct {
		id       int64
		distance float64
	}
	var items []item
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("scan fallback vector: %w", err)
		}
		var vector []float32
		if err := json.Unmarshal([]byte(raw), &vector); err != nil {
			return nil, fmt.Errorf("decode fallback vector %d: %w", id, err)
		}
		items = append(items, item{id: id, distance: euclidean(query, vector)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate fallback vectors: %w", err)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].distance == items[j].distance {
			return items[i].id < items[j].id
		}
		return items[i].distance < items[j].distance
	})
	if len(items) > limit {
		items = items[:limit]
	}
	results := make([]RankedResult, len(items))
	for i, item := range items {
		results[i] = RankedResult{ChunkID: item.id, Source: SourceVector, Rank: i + 1, Score: -item.distance}
	}
	return results, nil
}

func rankedToResults(ranked []RankedResult, source Source, limit int) []SearchResult {
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	results := make([]SearchResult, len(ranked))
	for i, item := range ranked {
		results[i] = SearchResult{
			ChunkID:     item.ChunkID,
			Source:      source,
			Score:       1 / float64(item.Rank),
			Explanation: fmt.Sprintf("%s#%d", item.Source, item.Rank),
		}
	}
	return results
}

func scanRanked(rows *sql.Rows, source Source) ([]RankedResult, error) {
	var results []RankedResult
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan %s result: %w", source, err)
		}
		results = append(results, RankedResult{ChunkID: id, Source: source, Rank: len(results) + 1})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s results: %w", source, err)
	}
	return results, nil
}

func scanRankedWithDistance(rows *sql.Rows, source Source) ([]RankedResult, error) {
	var results []RankedResult
	for rows.Next() {
		var id int64
		var distance float64
		if err := rows.Scan(&id, &distance); err != nil {
			return nil, fmt.Errorf("scan %s result: %w", source, err)
		}
		results = append(results, RankedResult{ChunkID: id, Source: source, Rank: len(results) + 1, Score: -distance})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s results: %w", source, err)
	}
	return results, nil
}

func hydrateResults(ctx context.Context, db *sql.DB, results []SearchResult) error {
	for i := range results {
		if err := db.QueryRowContext(ctx, `
			SELECT d.title, d.path, c.heading, c.text
			FROM chunks c
			JOIN documents d ON d.id = c.document_id
			WHERE c.id = ?
		`, results[i].ChunkID).Scan(&results[i].Title, &results[i].Path, &results[i].Heading, &results[i].Snippet); err != nil {
			return fmt.Errorf("hydrate chunk %d: %w", results[i].ChunkID, err)
		}
		// documents.path is the mkdocs nav breadcrumb and sometimes
		// embeds inline HTML (e.g. "<code>⎕FIX</code>: Fix Script"),
		// which leaks through both the CLI and the web UI. Strip the
		// tags so consumers see a plain "⎕FIX: Fix Script".
		results[i].Path = stripHTMLTags(results[i].Path)
		results[i].Snippet = compactSnippet(results[i].Snippet, 72)
	}
	return nil
}

var htmlTagRE = regexp.MustCompile(`<[^>]+>`)

func stripHTMLTags(s string) string {
	return htmlTagRE.ReplaceAllString(s, "")
}

func FormatResults(results []SearchResult) string {
	var b strings.Builder
	for _, result := range results {
		fmt.Fprintf(&b, "%d %s | %s | %s | %s | %s\n", result.ChunkID, result.Title, result.Path, result.Heading, result.Explanation, compactSnippet(result.Snippet, 72))
	}
	return b.String()
}

func quoteFTS(q string) string {
	return `"` + strings.ReplaceAll(q, `"`, `""`) + `"`
}

// markdownFencedBlock removes triple-backtick (or tilde) code fences,
// including the optional language tag, while keeping the code content.
var markdownFencedBlock = regexp.MustCompile("(?s)(?:```|~~~)[ \\t]*[A-Za-z0-9_+-]*[ \\t]*\\n?(.*?)\\n?(?:```|~~~)")

// markdownInlineCode strips single-backtick inline code wrappers but
// keeps the wrapped text.
var markdownInlineCode = regexp.MustCompile("`([^`]+)`")

// markdownLink matches [text](url) and leaves only text.
var markdownLink = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)

// markdownBold and markdownItalicUnderscore strip emphasis markers
// while keeping the wrapped text. We intentionally do not strip plain
// `*` because APL uses it for power, exponentiation, and signum.
var (
	markdownBold             = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	markdownItalicUnderscore = regexp.MustCompile(`\b_([^_\n]+)_\b`)
)

// markdownHeading strips leading # markers from headings.
var markdownHeading = regexp.MustCompile(`(?m)^\s{0,3}#{1,6}\s+`)

// markdownAdmonition strips mkdocs admonition openers from snippets.
// The leading "!!! warning \"Warning\"" line otherwise dominates the
// 72-char preview.
var markdownAdmonition = regexp.MustCompile(`(?m)^\s*!!!\s+\w+(?:\s+"[^"]*")?\s*$`)

// compactSnippet flattens chunk markdown into a single line of plain
// text suitable for an inline preview: code fences/inline code lose
// their backticks (keeping the code content), links collapse to their
// text, emphasis markers and heading hashes are stripped, and
// admonition openers are removed. Whitespace is then normalised and
// the result truncated to limit characters with an ellipsis.
func compactSnippet(s string, limit int) string {
	s = markdownAdmonition.ReplaceAllString(s, "")
	s = markdownFencedBlock.ReplaceAllString(s, " $1 ")
	s = markdownInlineCode.ReplaceAllString(s, "$1")
	s = markdownLink.ReplaceAllString(s, "$1")
	s = markdownBold.ReplaceAllString(s, "$1")
	s = markdownItalicUnderscore.ReplaceAllString(s, "$1")
	s = markdownHeading.ReplaceAllString(s, "")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= limit {
		return s
	}
	if limit < 4 {
		return s[:limit]
	}
	return strings.TrimSpace(s[:limit-3]) + "..."
}

func encodeVector(vector []float32) string {
	parts := make([]string, len(vector))
	for i, v := range vector {
		parts[i] = strconv.FormatFloat(float64(v), 'f', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func euclidean(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var sum float64
	for i := 0; i < n; i++ {
		diff := float64(a[i] - b[i])
		sum += diff * diff
	}
	return math.Sqrt(sum)
}

func HasSemanticTables(db *sql.DB) bool {
	var count int
	if err := db.QueryRow(`
		SELECT COUNT(*)
		FROM sqlite_schema
		WHERE name IN ('documents', 'chunks', 'chunks_fts', 'chunk_vec')
	`).Scan(&count); err != nil {
		return false
	}
	return count == 4
}

func IsSemanticMode(mode string) bool {
	switch Mode(mode) {
	case ModeFTS, ModeVector, ModeHybrid:
		return true
	default:
		return false
	}
}

func ContainsAPL(query string) bool {
	const aplChars = "⎕⍺⍵⍴⍳⍸⍷⍨⍥⍣⍤⍞⍝⍙⍕⍔⍒⍋⍉⍀⌿⍠⍟⌸⌺⌶⌹⌷⌈⌊∇¯¨∘⌾⍛⍜⊢⊣×÷⌽⊖⍪↑↓⊂⊃∊⍬⍎←⋄○∨∧∩∪~?!/\\,.:+="
	for _, r := range query {
		if unicode.In(r, unicode.Sk) || strings.ContainsRune(aplChars, r) {
			return true
		}
	}
	return false
}
