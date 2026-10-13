package main

import (
	"testing"

	"charm.land/bubbles/v2/help"

	"github.com/memmaker/gollum/llm"
)

// A resize while the pager owns the screen has to refit it too, not just the
// panes behind it.
func TestPagerResizeFollowsWindow(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New()}
	m.layout(100, 40)
	m.page = newMarkdownPager("t", "hello **world**", m.width, m.height)
	m.layout(60, 20)
	if got, want := m.page.vp.Width(), 59; got != want {
		t.Fatalf("width = %d, want %d", got, want)
	}
	if got, want := m.page.vp.Height(), 18; got != want {
		t.Fatalf("height = %d, want %d", got, want)
	}
}
