package markterm

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return ansiPattern.ReplaceAllString(s, "") }

func TestRenderMarkdown(t *testing.T) {
	source := "# Title\n\nSome **bold** and `code` and a [link](http://x.dev).\n\n" +
		"- first\n- second\n\n1. one\n2. two\n\n```go\nfmt.Println(\"hi\")\n```\n\n" +
		"> quoted\n\n| a | b |\n|---|---|\n| 1 | 2 |\n"
	got := RenderMarkdown(source, 80)

	// Every piece of content survives the renderer, styled or not.
	for _, want := range []string{
		"Title", "bold", "code", "link", "http://x.dev",
		"first", "second", "one", "two",
		`fmt.Println("hi")`, "quoted", "a", "1", "2",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered output lost %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, ansiBold) {
		t.Fatalf("bold text was not styled:\n%s", got)
	}
	plain := stripANSI(got)
	if !strings.Contains(plain, "• first") {
		t.Fatalf("bullet marker missing:\n%s", got)
	}
	if !strings.Contains(plain, "1. one") {
		t.Fatalf("ordered marker missing:\n%s", got)
	}
}

func TestVisibleLenIgnoresEscapes(t *testing.T) {
	if n := visibleLen(ansiBold + "abc" + ansiReset); n != 3 {
		t.Fatalf("visibleLen = %d, want 3", n)
	}
}

// Wide glyphs are two cells. Counting them as one used to wrap lines past the
// pane, where they overran the scrollbar column.
func TestWrapIndentCountsWideGlyphs(t *testing.T) {
	const width = 40
	src := strings.TrimSpace(strings.Repeat("🚀 漢字 ", 12))
	for _, line := range strings.Split(RenderMarkdown(src, width), "\n") {
		if w := ansi.StringWidth(line); w > width {
			t.Fatalf("line %d cells wide, pane is %d: %q", w, width, line)
		}
	}
}

func TestWrapIndentKeepsWordsWhole(t *testing.T) {
	got := wrapIndent("alpha beta gamma delta", 24, "> ")
	for _, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, "> ") {
			t.Fatalf("line missing indent: %q", line)
		}
		if visibleLen(line) > 24 {
			t.Fatalf("line too long (%d): %q", visibleLen(line), line)
		}
	}
	if strings.Contains(got, "gam\nma") {
		t.Fatalf("word was split: %q", got)
	}
}

// The width cap is what keeps long lines readable; lifting it must actually
// let the text use the pane.
func TestMaxWidth(t *testing.T) {
	defer func() { MaxWidth = DefaultMaxWidth }()
	source := strings.Repeat("word ", 60)

	MaxWidth = DefaultMaxWidth
	capped := widestLine(RenderMarkdown(source, 160))
	if capped > DefaultMaxWidth {
		t.Fatalf("capped render is %d wide, want <= %d", capped, DefaultMaxWidth)
	}

	MaxWidth = 0
	if got := widestLine(RenderMarkdown(source, 160)); got <= DefaultMaxWidth {
		t.Fatalf("uncapped render is %d wide, want more than %d", got, DefaultMaxWidth)
	}
}

func widestLine(s string) int {
	widest := 0
	for _, l := range strings.Split(stripANSI(s), "\n") {
		widest = max(widest, len([]rune(l)))
	}
	return widest
}

// A table only reads as a table when the column edges line up, which is what
// the padding is for: a wide cell in one row must not shift the next row's
// separator. Alignment declared in the delimiter row has to survive too.
func TestTableColumnsAlign(t *testing.T) {
	source := "| Level | What | Cost |\n|---|:---:|---:|\n" +
		"| lite | keeps full sentences | 3 |\n" +
		"| ultra | one word | 121 |\n"
	lines := strings.Split(strings.TrimRight(stripANSI(RenderMarkdown(source, 100)), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want header, rule and two rows:\n%s", len(lines), strings.Join(lines, "\n"))
	}

	// Every line puts its separators in the same columns.
	want := columnsOf(lines[0], '│')
	if len(want) != 2 {
		t.Fatalf("header has %d separators, want 2: %q", len(want), lines[0])
	}
	for _, l := range lines[2:] {
		if got := columnsOf(l, '│'); !slicesEqual(got, want) {
			t.Fatalf("separators at %v, want %v:\n%s", got, want, strings.Join(lines, "\n"))
		}
	}
	// The rule carries its junctions where the separators are.
	if got := columnsOf(lines[1], '┼'); !slicesEqual(got, want) {
		t.Fatalf("rule junctions at %v, want %v:\n%s", got, want, strings.Join(lines, "\n"))
	}

	// `---:` right-aligns, so the short number is pushed over under the long one.
	if !strings.Contains(lines[2], "  3") {
		t.Fatalf("right-aligned column not padded on the left: %q", lines[2])
	}
	// `:---:` centres, so the short cell has space on both sides.
	if !strings.Contains(lines[3], " one word ") {
		t.Fatalf("centred column not padded on both sides: %q", lines[3])
	}
}

// columnsOf reports the rune offsets of every occurrence of r.
func columnsOf(line string, r rune) []int {
	var at []int
	for i, c := range []rune(line) {
		if c == r {
			at = append(at, i)
		}
	}
	return at
}

func slicesEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
