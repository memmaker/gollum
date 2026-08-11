package main

import (
	"fmt"
	"strings"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Selection is by whole lines and belongs to one pane. Capturing the mouse
// takes the terminal's own selection away, and the terminal's selection could
// not respect a pane boundary anyway — it spans whatever columns it crosses.
//
// ponytail: lines, not characters. Character ranges need column mapping plus
// ANSI-aware slicing of every rendered row; add that if line copies prove too
// coarse.
type selection struct {
	pane   pane
	from   int  // first content line, absolute
	to     int  // last content line, absolute
	active bool // a drag is in progress
	set    bool
}

var selectStyle = lipgloss.NewStyle().Reverse(true)

// covers reports whether an absolute content line of pane p is selected.
func (s selection) covers(p pane, line int) bool {
	if !s.set || s.pane != p {
		return false
	}
	lo, hi := s.from, s.to
	if lo > hi {
		lo, hi = hi, lo
	}
	return line >= lo && line <= hi
}

// highlight restyles the selected lines of content belonging to pane p.
func (s selection) highlight(content string, p pane) string {
	if !s.set || s.pane != p {
		return content
	}
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		if s.covers(p, i) {
			// Styling inside the line would cancel the reverse video part way
			// through, so a selected line is shown as plain reversed text.
			lines[i] = selectStyle.Render(ansi.Strip(l))
		}
	}
	return strings.Join(lines, "\n")
}

func (m *tui) viewportFor(p pane) *viewport.Model {
	if p == paneSidebar {
		return &m.side
	}
	return &m.out
}

func (m *tui) contentFor(p pane) string {
	if p == paneSidebar {
		return m.sideContent
	}
	return m.transcript.String() + m.answer.String()
}

// paneAt maps a screen cell to the pane holding it and the row within it.
// The scrollbar column belongs to neither.
func (m *tui) paneAt(x, y int) (pane, int, bool) {
	var p pane
	switch {
	case x < m.out.Width:
		p = paneOutput
	case m.width >= 90 && x > m.out.Width:
		p = paneSidebar
	default:
		return 0, 0, false
	}
	top, height := m.paneTop(p), m.viewportFor(p).Height
	if y < top || y >= top+height {
		return 0, 0, false
	}
	return p, y - top, true
}

// paneTop is the first screen row a pane draws on. The sidebar starts below
// the logo, which is fixed and outside its viewport.
func (m *tui) paneTop(p pane) int {
	if p == paneSidebar && m.logo != "" {
		return animHeight + lipgloss.Height(m.logo)
	}
	return animHeight
}

// mouse handles wheel scrolling and line selection. The wheel keeps its
// meaning: it scrolls whatever has focus.
func (m *tui) mouse(msg tea.MouseMsg) tea.Cmd {
	if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
		vp := m.viewportFor(m.focus)
		updated, cmd := vp.Update(msg)
		*vp = updated
		return cmd
	}

	switch msg.Action {
	case tea.MouseActionPress:
		if msg.Button != tea.MouseButtonLeft {
			return nil
		}
		p, row, ok := m.paneAt(msg.X, msg.Y)
		if !ok {
			return nil
		}
		m.focus = p
		m.input.Blur()
		line := m.viewportFor(p).YOffset + row
		m.sel = selection{pane: p, from: line, to: line, active: true, set: true}

	case tea.MouseActionMotion:
		if !m.sel.active {
			return nil
		}
		// A drag leaving the pane keeps extending along its edge instead of
		// jumping to whatever the pointer is over.
		vp := m.viewportFor(m.sel.pane)
		row := min(max(msg.Y-m.paneTop(m.sel.pane), 0), vp.Height-1)
		m.sel.to = vp.YOffset + row

	case tea.MouseActionRelease:
		if !m.sel.active {
			return nil
		}
		m.sel.active = false
		m.copySelection()
	}
	m.redraw()
	return nil
}

// copySelection puts the selected lines on the system clipboard.
func (m *tui) copySelection() {
	m.note = copyLines(m.contentFor(m.sel.pane), m.sel.from, m.sel.to)
}

// copyLines copies an inclusive line range of content to the system clipboard,
// stripped of styling — nobody wants escape codes pasted into an editor. It
// returns the message to show the user.
func copyLines(content string, from, to int) string {
	lines := strings.Split(content, "\n")
	lo, hi := from, to
	if lo > hi {
		lo, hi = hi, lo
	}
	lo, hi = max(lo, 0), min(hi, len(lines)-1)
	if lo > hi {
		return ""
	}
	out := make([]string, 0, hi-lo+1)
	for _, l := range lines[lo : hi+1] {
		out = append(out, strings.TrimRight(ansi.Strip(l), " "))
	}
	if err := clipboard.WriteAll(strings.Join(out, "\n")); err != nil {
		return "clipboard: " + err.Error()
	}
	return fmt.Sprintf("copied %d line(s)", len(out))
}

// clearSelection drops the highlight, e.g. once a key is pressed.
func (m *tui) clearSelection() {
	if !m.sel.set {
		return
	}
	m.sel = selection{}
	m.redraw()
}
