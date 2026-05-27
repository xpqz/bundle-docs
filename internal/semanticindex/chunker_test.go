//go:build semantic

package semanticindex

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChunkMarkdownSplitsByHeadingsAndKeepsMetadata(t *testing.T) {
	doc := SourceDocument{
		Path:    "language/functions.md",
		Title:   "Functions",
		Content: "# Functions\n\nIntro text.\n\n## Tradfns\n\nTradfn body.\n\n### Guards\n\nGuard body.\n\n## Dfns\n\nDfn body.",
	}

	chunks := ChunkMarkdown(doc, ChunkOptions{MaxTokens: 80})
	if len(chunks) != 3 {
		t.Fatalf("len(chunks) = %d, want 3", len(chunks))
	}

	want := []struct {
		ordinal int
		heading string
		anchor  string
		text    string
	}{
		{0, "Functions", "functions", "Intro text."},
		{1, "Tradfns / Guards", "tradfns-guards", "Tradfn body.\n\nGuard body."},
		{2, "Dfns", "dfns", "Dfn body."},
	}
	for i, w := range want {
		if chunks[i].Ordinal != w.ordinal {
			t.Fatalf("chunk %d ordinal = %d, want %d", i, chunks[i].Ordinal, w.ordinal)
		}
		if chunks[i].Heading != w.heading {
			t.Fatalf("chunk %d heading = %q, want %q", i, chunks[i].Heading, w.heading)
		}
		if chunks[i].Anchor != w.anchor {
			t.Fatalf("chunk %d anchor = %q, want %q", i, chunks[i].Anchor, w.anchor)
		}
		if chunks[i].Text != w.text {
			t.Fatalf("chunk %d text = %q, want %q", i, chunks[i].Text, w.text)
		}
		if chunks[i].DocumentPath != doc.Path || chunks[i].DocumentTitle != doc.Title {
			t.Fatalf("chunk %d lost document metadata: %#v", i, chunks[i])
		}
	}
}

func TestChunkMarkdownSplitsLongSectionsByParagraphBudget(t *testing.T) {
	doc := SourceDocument{
		Path:    "language/arrays.md",
		Title:   "Arrays",
		Content: "# Arrays\n\none two three four five\n\nsix seven eight nine ten\n\neleven twelve thirteen fourteen fifteen",
	}

	chunks := ChunkMarkdown(doc, ChunkOptions{MaxTokens: 6})
	if len(chunks) != 3 {
		t.Fatalf("len(chunks) = %d, want 3", len(chunks))
	}
	for i, chunk := range chunks {
		if chunk.Heading != "Arrays" {
			t.Fatalf("chunk %d heading = %q, want Arrays", i, chunk.Heading)
		}
		if chunk.Anchor == "" {
			t.Fatalf("chunk %d has empty anchor", i)
		}
		if chunk.Text == "" {
			t.Fatalf("chunk %d has empty text", i)
		}
	}
	if chunks[1].Anchor == chunks[0].Anchor || chunks[2].Anchor == chunks[0].Anchor {
		t.Fatalf("split chunk anchors are not unique: %#v", chunks)
	}
}

func TestChunkMarkdownIgnoresHeadingsInsideCodeFences(t *testing.T) {
	doc := SourceDocument{
		Path:    "language/examples.md",
		Title:   "Examples",
		Content: "# Examples\n\nBefore.\n\n```apl\n# Not a heading\n⎕FIX 'f←{⍵}'\n```\n\n## Real Heading\n\nAfter.",
	}

	chunks := ChunkMarkdown(doc, DefaultChunkOptions())
	if len(chunks) != 2 {
		t.Fatalf("len(chunks) = %d, want 2: %#v", len(chunks), chunks)
	}
	if chunks[0].Heading != "Examples" {
		t.Fatalf("first heading = %q, want Examples", chunks[0].Heading)
	}
	if !contains(chunks[0].Text, "# Not a heading") || !contains(chunks[0].Text, "⎕FIX") {
		t.Fatalf("first chunk lost fenced APL content: %q", chunks[0].Text)
	}
	if chunks[1].Heading != "Real Heading" {
		t.Fatalf("second heading = %q, want Real Heading", chunks[1].Heading)
	}
}

func TestDefaultChunkOptionsUsePlannedTokenBudget(t *testing.T) {
	options := DefaultChunkOptions()
	if options.MaxTokens < 300 || options.MaxTokens > 800 {
		t.Fatalf("DefaultChunkOptions MaxTokens = %d, want 300-800", options.MaxTokens)
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

func TestEmbeddingTextClampsOversizedChunks(t *testing.T) {
	// A chunk whose body is a giant HTML table: few "tokens" but
	// tens of thousands of characters. Must come back clamped under
	// the embedder's input cap.
	body := strings.Repeat("<td class=\"Dyalog\">x</td>\n", 5000) // ~125k chars
	chunk := MarkdownChunk{
		DocumentTitle: "Format Date-time R←X(1200⌶)Y",
		Heading:       "Formatting Pattern",
		Text:          body,
	}
	got := EmbeddingText(chunk)
	if n := len([]rune(got)); n > MaxEmbeddingTextChars {
		t.Fatalf("EmbeddingText returned %d chars, want <= %d", n, MaxEmbeddingTextChars)
	}
	// The canonical prefix (title) must survive the clamp.
	if !strings.HasPrefix(got, "Format Date-time R←X(1200⌶)Y") {
		t.Fatalf("clamp dropped the title prefix: %q...", got[:40])
	}
}

func TestClampCharsNeverSplitsRunes(t *testing.T) {
	// All multi-byte runes; clamping must not produce invalid UTF-8.
	s := strings.Repeat("⎕", 100)
	got := clampChars(s, 10)
	if utf8len := len([]rune(got)); utf8len != 10 {
		t.Fatalf("clampChars rune count = %d, want 10", utf8len)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("clampChars produced invalid UTF-8: %q", got)
	}
}

func TestEmbeddingTextPrependsTitleAndHeading(t *testing.T) {
	cases := []struct {
		name    string
		chunk   MarkdownChunk
		want    string
	}{
		{
			name: "title and distinct heading both prepended",
			chunk: MarkdownChunk{
				DocumentTitle: "Execute R←⍎Y",
				Heading:       "Examples",
				Text:          "⍎'2+2'",
			},
			want: "Execute R←⍎Y\nExamples\n⍎'2+2'",
		},
		{
			name: "heading omitted when it duplicates the title",
			chunk: MarkdownChunk{
				DocumentTitle: "Where R←⍸Y",
				Heading:       "Where R←⍸Y",
				Text:          "Classic Edition note.",
			},
			want: "Where R←⍸Y\nClassic Edition note.",
		},
		{
			name: "heading omitted when it is a substring of the title",
			chunk: MarkdownChunk{
				DocumentTitle: "Fix Script {R}←{X}⎕FIX Y",
				Heading:       "Fix Script",
				Text:          "Body.",
			},
			want: "Fix Script {R}←{X}⎕FIX Y\nBody.",
		},
		{
			name: "missing title falls back to heading + text",
			chunk: MarkdownChunk{
				Heading: "Standalone",
				Text:    "Body.",
			},
			want: "Standalone\nBody.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EmbeddingText(tc.chunk); got != tc.want {
				t.Fatalf("EmbeddingText = %q, want %q", got, tc.want)
			}
		})
	}
}
