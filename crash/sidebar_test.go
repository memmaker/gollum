package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/x/ansi"

	"github.com/memmaker/gollum/llm"
)

// Nothing in the sidebar may run past its edge, not even an unbroken word: a
// model slug is one long token and used to be cut off mid-name.
func TestSidebarWraps(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New(),
		current: strings.Repeat("x", 60)}
	m.layout(100, 24)
	m.note = "error: some backend said something quite long about a token"
	m.refreshSidebar()

	lines := strings.Split(m.sideContent, "\n")
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > m.side.Width {
			t.Fatalf("line too wide (%d > %d): %q", w, m.side.Width, ansi.Strip(l))
		}
	}
	// The long slug is folded, not truncated: every x survives. Counted above
	// the cwd line only — the working directory is whatever the test host says
	// it is, and its letters are none of this test's business.
	head, _, _ := strings.Cut(ansi.Strip(m.sideContent), "cwd")
	if got := strings.Count(head, "x"); got != 60 {
		t.Fatalf("kept %d of 60 characters", got)
	}
}
