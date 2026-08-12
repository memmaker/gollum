package main

import (
	"testing"

	"charm.land/bubbles/v2/help"
	tea "charm.land/bubbletea/v2"

	"github.com/memmaker/gollum/llm"
)

// A bracketed paste has to reach the prompt in both shapes, and a multiline
// paste has to survive the one-row box's fold.
func TestPaste(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New()}
	m.focusInput()

	m.paste(tea.PasteMsg{Content: "one line"})
	if got := m.value(); got != "one line" {
		t.Fatalf("one-line paste = %q, want %q", got, "one line")
	}

	m.resetInput()
	m.paste(tea.PasteMsg{Content: "a\nb"})
	if got := m.value(); got != "a\nb" {
		t.Fatalf("folded paste = %q, want %q", got, "a\nb")
	}

	m.multiline = true
	m.multiLine.Reset()
	m.focusInput()
	m.paste(tea.PasteMsg{Content: "a\nb"})
	if got := m.value(); got != "a\nb" {
		t.Fatalf("multiline paste = %q, want %q", got, "a\nb")
	}
}

// The multiline box's height is the only non-trivial arithmetic the input
// mode adds: grow with content, floor at 3, and never crowd the animation
// bar, the output pane's own floor, the box's own border rows, or the help
// line off a short terminal.
func TestInputHeight(t *testing.T) {
	cases := []struct {
		name    string
		wrapped int
		term    int
		want    int
	}{
		{"empty content rests at the floor", 0, 40, 3},
		{"one line rests at the floor", 1, 40, 3},
		{"grows with content", 4, 40, 4},
		{"capped before it crowds the frame", 200, 40, 33}, // 40-1-3-2-1
		{"tiny terminal shrinks below the floor", 4, 9, 2}, // 9-1-3-2-1
		{"terminal too short even for one row floors at 1", 1, 6, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := inputHeight(c.wrapped, c.term); got != c.want {
				t.Fatalf("inputHeight(%d, %d) = %d, want %d", c.wrapped, c.term, got, c.want)
			}
		})
	}
}

func TestWrappedRows(t *testing.T) {
	cases := []struct {
		name  string
		s     string
		width int
		want  int
	}{
		{"empty string is still one row", "", 20, 1},
		{"short line is one row", "hello", 20, 1},
		{"exact width is one row", "0123456789", 10, 1},
		{"one over the width wraps to two", "01234567890", 10, 2},
		{"newlines count separately", "a\nb\nc", 20, 3},
		{"wide glyphs count cells, not runes", "🚀🚀🚀🚀🚀🚀", 10, 2}, // 12 cells / 10
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := wrappedRows(c.s, c.width); got != c.want {
				t.Fatalf("wrappedRows(%q, %d) = %d, want %d", c.s, c.width, got, c.want)
			}
		})
	}
}
