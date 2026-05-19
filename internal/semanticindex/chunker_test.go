//go:build fts5

package semanticindex

import (
	"strings"
	"testing"
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
