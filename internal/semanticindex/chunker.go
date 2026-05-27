//go:build semantic

package semanticindex

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

type SourceDocument struct {
	Path    string
	File    string
	Title   string
	Content string
}

type ChunkOptions struct {
	MaxTokens int
}

type MarkdownChunk struct {
	DocumentPath  string
	DocumentTitle string
	Ordinal       int
	Heading       string
	Anchor        string
	Text          string
	ContentHash   string
}

func DefaultChunkOptions() ChunkOptions {
	return ChunkOptions{MaxTokens: 500}
}

// MaxEmbeddingTextChars bounds the length (in Unicode code points) of
// the string handed to the embedder. The chunker splits by token
// (whitespace-separated word) budget, but a dense HTML table has few
// words and many characters, so a chunk can sit under the token
// budget yet run to tens of thousands of characters. The embedding
// model (BAAI/bge-small-en-v1.5) only consumes ~512 tokens, so the
// tail is wasted anyway; clamping here keeps us comfortably under the
// embedding server's per-text input cap (8192) without losing signal
// the model would have used.
const MaxEmbeddingTextChars = 8000

// EmbeddingText is the string sent to the embedder for a chunk.
//
// The chunk body alone is often missing the canonical name of the page
// (e.g. the body of "Execute R←⍎Y" starts with "!!! Warning" and never
// mentions the word "execute"). Prepending the document title and the
// section heading gives the model the symbol and the plain-English name
// to anchor natural-language queries against. The heading is omitted
// when it is identical to the title or a substring of it.
//
// The result is clamped to MaxEmbeddingTextChars on a rune boundary so
// chunks with large embedded HTML (tables, etc.) don't blow past the
// embedder's input cap.
func EmbeddingText(chunk MarkdownChunk) string {
	var b strings.Builder
	title := strings.TrimSpace(chunk.DocumentTitle)
	heading := strings.TrimSpace(chunk.Heading)
	if title != "" {
		b.WriteString(title)
		b.WriteByte('\n')
	}
	if heading != "" && heading != title && !strings.Contains(title, heading) {
		b.WriteString(heading)
		b.WriteByte('\n')
	}
	b.WriteString(chunk.Text)
	return clampChars(b.String(), MaxEmbeddingTextChars)
}

// clampChars truncates s to at most maxChars Unicode code points,
// never splitting a multi-byte rune.
func clampChars(s string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	return string(runes[:maxChars])
}

func ChunkMarkdown(doc SourceDocument, options ChunkOptions) []MarkdownChunk {
	if options.MaxTokens <= 0 {
		options = DefaultChunkOptions()
	}

	sections := splitSections(doc)
	var chunks []MarkdownChunk
	for _, section := range sections {
		text := normalizeChunkText(section.text)
		if text == "" {
			continue
		}
		parts := splitByTokenBudget(text, options.MaxTokens)
		for i, part := range parts {
			anchor := section.anchor
			if len(parts) > 1 {
				anchor = fmt.Sprintf("%s-%d", anchor, i+1)
			}
			chunk := MarkdownChunk{
				DocumentPath:  doc.Path,
				DocumentTitle: doc.Title,
				Ordinal:       len(chunks),
				Heading:       section.heading,
				Anchor:        anchor,
				Text:          strings.TrimSpace(part),
			}
			chunk.ContentHash = hashText(chunk.Text)
			chunks = append(chunks, chunk)
		}
	}
	return chunks
}

type markdownSection struct {
	heading string
	anchor  string
	text    string
}

func splitSections(doc SourceDocument) []markdownSection {
	lines := strings.Split(doc.Content, "\n")
	var sections []markdownSection
	var current *markdownSection
	var headingStack []string
	inFence := false

	flush := func() {
		if current == nil {
			return
		}
		current.text = strings.TrimSpace(current.text)
		if current.text != "" {
			sections = append(sections, *current)
		}
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
		}
		if !inFence {
			if level, heading, ok := parseHeading(line); ok {
				if level <= 2 {
					flush()
					headingStack = setHeading(headingStack, level, heading)
					current = &markdownSection{heading: heading, anchor: slugify(heading)}
					if level == 1 && strings.TrimSpace(heading) == "" {
						current.heading = fallbackHeading(doc)
						current.anchor = slugify(fallbackHeading(doc))
					}
					continue
				}
				headingStack = setHeading(headingStack, level, heading)
				if current == nil {
					current = &markdownSection{
						heading: fallbackHeading(doc),
						anchor:  slugify(fallbackHeading(doc)),
					}
				}
				current.heading = strings.Join(headingStack[max(0, len(headingStack)-2):], " / ")
				current.anchor = slugify(strings.Join(headingStack[max(0, len(headingStack)-2):], " "))
				continue
			}
		}
		if current == nil {
			current = &markdownSection{
				heading: fallbackHeading(doc),
				anchor:  slugify(fallbackHeading(doc)),
			}
		}
		current.text += line + "\n"
	}
	flush()
	return sections
}

func parseHeading(line string) (int, string, bool) {
	trimmed := strings.TrimLeft(line, " ")
	if !strings.HasPrefix(trimmed, "#") {
		return 0, "", false
	}
	hashes := 0
	for hashes < len(trimmed) && trimmed[hashes] == '#' {
		hashes++
	}
	if hashes == 0 || hashes > 6 || hashes >= len(trimmed) || trimmed[hashes] != ' ' {
		return 0, "", false
	}
	return hashes, strings.TrimSpace(trimmed[hashes:]), true
}

func setHeading(stack []string, level int, heading string) []string {
	if level <= 0 {
		level = 1
	}
	if len(stack) >= level {
		stack = stack[:level-1]
	}
	for len(stack) < level-1 {
		stack = append(stack, "")
	}
	return append(stack, heading)
}

func fallbackHeading(doc SourceDocument) string {
	if doc.Title != "" {
		return doc.Title
	}
	if doc.Path != "" {
		return doc.Path
	}
	return "Document"
}

func splitByTokenBudget(text string, maxTokens int) []string {
	if tokenCount(text) <= maxTokens {
		return []string{text}
	}

	paragraphs := regexp.MustCompile(`\n\s*\n`).Split(text, -1)
	var out []string
	var current []string
	currentTokens := 0
	for _, paragraph := range paragraphs {
		paragraph = strings.TrimSpace(paragraph)
		if paragraph == "" {
			continue
		}
		paragraphTokens := tokenCount(paragraph)
		if currentTokens > 0 && currentTokens+paragraphTokens > maxTokens {
			out = append(out, strings.Join(current, "\n\n"))
			current = nil
			currentTokens = 0
		}
		current = append(current, paragraph)
		currentTokens += paragraphTokens
	}
	if len(current) > 0 {
		out = append(out, strings.Join(current, "\n\n"))
	}
	return out
}

func normalizeChunkText(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.TrimSpace(regexp.MustCompile(`\n{3,}`).ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

func tokenCount(text string) int {
	return len(strings.Fields(text))
}

func slugify(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func hashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return fmt.Sprintf("%x", sum[:])
}

func encodeVectorJSON(vector []float32) string {
	parts := make([]string, len(vector))
	for i, v := range vector {
		parts[i] = strconv.FormatFloat(float64(v), 'f', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
