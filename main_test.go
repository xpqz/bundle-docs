package main

import "testing"

func TestExtractMarkdownH1FindsFirstHeading(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "basic h1",
			in:   "# Hello\n\nbody text",
			want: "Hello",
		},
		{
			name: "h1 with inline code (backticks kept verbatim)",
			in:   "# Format `⎕FMT` Function\n\nbody",
			want: "Format `⎕FMT` Function",
		},
		{
			name: "h2 alone is ignored",
			in:   "## Section\n\nbody",
			want: "",
		},
		{
			name: "or.md regression: bare # inside fenced code does not become title",
			in: "<div style=\"display:none\">⎕OR</div>\n" +
				"\n" +
				"```apl\n" +
				"      )CS\n" +
				"#\n" +
				"      'ORTEST' ⎕FCREATE 1\n" +
				"```\n" +
				"\n" +
				"# Object Representation R←⎕OR Y\n" +
				"\n" +
				"body",
			want: "Object Representation R←⎕OR Y",
		},
		{
			name: "html-only h1 returns empty",
			in:   "<h1>HTML Heading</h1>\n\nbody",
			want: "",
		},
		{
			name: "front matter not stripped here (caller's job) but no h1 means empty",
			in:   "---\nfoo: 1\n---\n\nbody only, no heading",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractMarkdownH1(tc.in); got != tc.want {
				t.Fatalf("extractMarkdownH1:\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

func TestExtractTitleAndCleanPicksMarkdownH1OverFencedNoise(t *testing.T) {
	// Mirrors the actual or.md shape: front matter, hidden div, HTML
	// h1. The bug used to extract a code-block line as title.
	raw := []byte("---\n" +
		"search:\n" +
		"  boost: 2\n" +
		"---\n" +
		"<!-- Hidden search keywords -->\n" +
		"<div style=\"display: none;\">\n" +
		"  ⎕OR OR\n" +
		"</div>\n" +
		"\n" +
		"```apl\n" +
		"      )CS\n" +
		"#\n" +
		"      'ORTEST' ⎕FCREATE 1\n" +
		"```\n" +
		"\n" +
		"<h1 class=\"heading\"><span class=\"name\">Object Representation</span> <span class=\"command\">R←⎕OR Y</span></h1>\n" +
		"\n" +
		"`⎕OR` converts a defined function...\n")
	title, _, _ := extractTitleAndClean(raw)
	want := "Object Representation R←⎕OR Y"
	if title != want {
		t.Fatalf("title = %q, want %q", title, want)
	}
}

func TestExtractTitleAndCleanPrefersHTMLH1WhenBothPresent(t *testing.T) {
	// Dyalog docs use HTML <h1> as the canonical title (3043 of 3090
	// pages), so HTML wins over markdown when both are present.
	raw := []byte("# Markdown Title\n\n<h1>HTML Title</h1>\n\nbody")
	title, _, _ := extractTitleAndClean(raw)
	if title != "HTML Title" {
		t.Fatalf("title = %q, want HTML Title", title)
	}
}

func TestExtractTitleAndCleanFallsBackToMarkdownH1WhenNoHTMLH1(t *testing.T) {
	raw := []byte("# Markdown Only Title\n\nbody")
	title, _, _ := extractTitleAndClean(raw)
	if title != "Markdown Only Title" {
		t.Fatalf("title = %q, want Markdown Only Title", title)
	}
}
