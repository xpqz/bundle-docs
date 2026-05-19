//go:build fts5

package semanticsearch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
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

	var fts []RankedResult
	var vector []RankedResult
	var err error
	if options.Mode == ModeFTS || options.Mode == ModeHybrid {
		fts, err = searchFTS(ctx, db, options.Query, options.Limit)
		if err != nil {
			return nil, err
		}
	}
	if options.Mode == ModeVector || options.Mode == ModeHybrid {
		if embedder == nil {
			return nil, fmt.Errorf("embedding service is required for %s mode", options.Mode)
		}
		vector, err = searchVector(ctx, db, embedder, options)
		if err != nil {
			return nil, err
		}
	}

	var fused []SearchResult
	switch options.Mode {
	case ModeFTS:
		fused = rankedToResults(fts, SourceFTS, options.Limit)
	case ModeVector:
		fused = rankedToResults(vector, SourceVector, options.Limit)
	default:
		fused = FuseResults(fts, vector, WeightsForQuery(options.Query), options.Limit)
	}
	if err := hydrateResults(ctx, db, fused); err != nil {
		return nil, err
	}
	return fused, nil
}

func searchFTS(ctx context.Context, db *sql.DB, query string, limit int) ([]RankedResult, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT rowid
		FROM chunks_fts
		WHERE chunks_fts MATCH ?
		ORDER BY bm25(chunks_fts), rowid
		LIMIT ?
	`, quoteFTS(query), limit)
	if err != nil {
		return nil, fmt.Errorf("semantic FTS search: %w", err)
	}
	defer rows.Close()
	return scanRanked(rows, SourceFTS)
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
		results[i].Snippet = compactSnippet(results[i].Snippet, 72)
	}
	return nil
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

func compactSnippet(s string, limit int) string {
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
