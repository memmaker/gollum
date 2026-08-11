package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestRenderLogo(t *testing.T) {
	lines := strings.Split(renderLogo("CRASH", 34, false), "\n")
	if len(lines) != logoHeight {
		t.Fatalf("lines = %d, want %d", len(lines), logoHeight)
	}
	// The logo keeps its own width, it does not stretch to the pane.
	for i, l := range lines {
		if w := ansi.StringWidth(l); w != logoWidth {
			t.Fatalf("line %d width = %d, want %d: %q", i, w, logoWidth, ansi.Strip(l))
		}
	}
	// Diagonal fields top and bottom, letters in between.
	if got := ansi.Strip(lines[0]); strings.Trim(got, diag) != "" {
		t.Fatalf("first line = %q, want a diagonal field", got)
	}
	if got := ansi.Strip(lines[2]); !strings.HasPrefix(got, tagline) {
		t.Fatalf("tagline row = %q", got)
	}
	if got := ansi.Strip(lines[3]); !strings.HasPrefix(got, "▄▀▀▀▀ ") {
		t.Fatalf("wordmark = %q", got)
	}
	// Too narrow for the word: nothing at all rather than a cut-off logo.
	if got := renderLogo("CRASH", logoWidth-1, false); got != "" {
		t.Fatalf("narrow render = %q, want empty", got)
	}
}

// The tagline row is one line of the logo block, so it has to stay exactly
// logoWidth wide whether or not OBLIVIUM shares it — a row that grows pushes
// the sidebar out from under the wordmark.
func TestTaglineRowWidth(t *testing.T) {
	for _, forgetful := range []bool{false, true} {
		row := strings.Split(renderLogo("CRASH", logoWidth, forgetful), "\n")[2]
		if got := ansi.StringWidth(row); got != logoWidth {
			t.Fatalf("forgetful=%v: row is %d wide, want %d: %q", forgetful, got, logoWidth, ansi.Strip(row))
		}
		plain := ansi.Strip(row)
		if !strings.HasPrefix(plain, tagline) {
			t.Fatalf("row does not start with the tagline: %q", plain)
		}
		if got := strings.HasSuffix(plain, oblivium); got != forgetful {
			t.Fatalf("forgetful=%v but OBLIVIUM present=%v: %q", forgetful, got, plain)
		}
	}
}
