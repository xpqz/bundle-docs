//go:build semantic

package main

import (
	"strings"
	"testing"
)

func TestPreprocessAdmonitionsRewritesToBlockquotes(t *testing.T) {
	in := strings.Join([]string{
		"!!! Warning \"Be careful\"",
		"    Untrusted input is risky. Use ⎕VGET instead:",
		"",
		"    * vget for variables",
		"    * vfi for numbers",
		"",
		"After the admonition.",
	}, "\n")
	want := strings.Join([]string{
		"> **Be careful**",
		"> Untrusted input is risky. Use ⎕VGET instead:",
		">",
		"> * vget for variables",
		"> * vfi for numbers",
		"",
		"After the admonition.",
	}, "\n")
	if got := preprocessAdmonitions(in); got != want {
		t.Fatalf("preprocessAdmonitions:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestPreprocessAdmonitionsDefaultsTitleFromTypeWhenAbsent(t *testing.T) {
	in := "!!! note\n    Body line."
	want := "> **Note**\n> Body line."
	if got := preprocessAdmonitions(in); got != want {
		t.Fatalf("preprocessAdmonitions: got %q, want %q", got, want)
	}
}

func TestRewriteRelativeLinksTurnsCrossRefsIntoAbsoluteURLs(t *testing.T) {
	source := "language-reference-guide/docs/primitive-functions/execute.md"
	in := `prose <a href="../system-functions/vget.md"><code>⎕VGET</code></a> and ` +
		`<a href="./tally.md#tally-r-y">Tally</a> plus ` +
		`<a href="https://example.com/x.md">external</a>.`
	got := rewriteRelativeLinks(in, source)
	wants := []string{
		`href="https://dyalog.github.io/documentation/20.0/language-reference-guide/system-functions/vget" target="_blank"`,
		`href="https://dyalog.github.io/documentation/20.0/language-reference-guide/primitive-functions/tally#tally-r-y" target="_blank"`,
		`href="https://example.com/x.md">external`,
	}
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Fatalf("rewriteRelativeLinks missing %q in:\n%s", want, got)
		}
	}
}

func TestRewriteRelativeLinksIsNoopWithoutSourceFile(t *testing.T) {
	in := `<a href="foo.md">x</a>`
	if got := rewriteRelativeLinks(in, ""); got != in {
		t.Fatalf("empty sourceFile should be no-op: got %q", got)
	}
}

func TestSourceURLStripsDocsAndMdAndAppendsAnchor(t *testing.T) {
	cases := []struct {
		name   string
		file   string
		anchor string
		want   string
	}{
		{
			name:   "primitive function with anchor",
			file:   "language-reference-guide/docs/primitive-functions/execute.md",
			anchor: "execute-r-y",
			want:   "https://dyalog.github.io/documentation/20.0/language-reference-guide/primitive-functions/execute#execute-r-y",
		},
		{
			name:   "system function without anchor",
			file:   "language-reference-guide/docs/system-functions/fix.md",
			anchor: "",
			want:   "https://dyalog.github.io/documentation/20.0/language-reference-guide/system-functions/fix",
		},
		{
			name:   "windows guide path",
			file:   "windows-installation-and-configuration-guide/docs/configuration-parameters/log-file-inuse.md",
			anchor: "",
			want:   "https://dyalog.github.io/documentation/20.0/windows-installation-and-configuration-guide/configuration-parameters/log-file-inuse",
		},
		{
			name:   "empty file returns empty",
			file:   "",
			anchor: "anchor",
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sourceURL(tc.file, tc.anchor); got != tc.want {
				t.Fatalf("sourceURL(%q, %q) = %q, want %q", tc.file, tc.anchor, got, tc.want)
			}
		})
	}
}
