package main

import (
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// The prompt has two shapes. Most of the time it is a single row — a
// textinput scrolls horizontally rather than wrap, so a long line stays one
// line, same as a shell. ctrl+space swaps in a textarea that grows with the
// content, for a prompt worth seeing in full before it is sent. Its rounded
// border stands in for the delimiter a plain rule would have drawn, and
// spends the same rows and columns a rule plus its own edges would have.
const (
	oneLineHeight       = 1 // the default prompt: one row, no wrap
	multilineBorderRows = 2 // the box's own top and bottom border edges
	multilineBorderCols = 2 // the box's own left and right border edges
	minMultilineHeight  = 3 // floor the box's content rests at, border excluded
	outputFloor         = 3 // rows the output pane keeps even at max multiline height
)

// inputPlaceholder hints at the slash commands from whichever box is empty.
const inputPlaceholder = "ask something — /model, /debug, /exit"

// newOneLineInput builds the default prompt.
func newOneLineInput() textinput.Model {
	ti := textinput.New()
	ti.Placeholder = inputPlaceholder
	return ti
}

// newMultiLineInput builds the box ctrl+space opens. Enter keeps its textarea
// default of inserting a newline; alt+enter sends, so a box that looks like
// an editor acts like one.
func newMultiLineInput() textarea.Model {
	ta := textarea.New()
	ta.Placeholder = inputPlaceholder
	ta.ShowLineNumbers = false
	// The border already marks the box's edge; a prompt glyph on every line
	// would just repeat that down the left side.
	ta.Prompt = ""
	ta.SetHeight(minMultilineHeight)
	// The default cursor line paints its own background, which shows up as a
	// block behind the placeholder. Let the terminal background through.
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.BlurredStyle.CursorLine = lipgloss.NewStyle()
	return ta
}

// newlineFold stands in for a line break when multiline text is folded into
// the one-row box. textinput cannot hold a real newline — its own sanitizer
// silently turns one into a space — and the box is one row regardless, so
// there is no "scroll to this line" to offer. The fold is reversible: value
// hands the original text back as long as the box was not edited since.
const newlineFold = " ↵ "

// value returns the text in whichever input widget is active, unfolding a
// multiline draft that was toggled down and left untouched.
func (m *tui) value() string {
	if m.multiline {
		return m.multiLine.Value()
	}
	if v := m.oneLine.Value(); m.foldedDisplay != "" && v == m.foldedDisplay {
		return m.foldedOrig
	}
	return m.oneLine.Value()
}

// setValue replaces the active widget's text, cursor at the end. A history
// entry can carry real newlines (history.go says so), so the one-row box
// folds it exactly as toggling down would.
func (m *tui) setValue(s string) {
	if m.multiline {
		m.multiLine.SetValue(s)
		m.multiLine.CursorEnd()
		return
	}
	m.setOneLine(s)
}

// setOneLine puts s in the one-row box, folding any newlines behind
// newlineFold rather than handing them to textinput's own sanitizer, which
// silently turns each into a space. value unfolds it again, exactly, as long
// as the box goes untouched in between.
func (m *tui) setOneLine(s string) {
	m.foldedDisplay, m.foldedOrig = "", ""
	folded := s
	if strings.Contains(s, "\n") {
		folded = strings.ReplaceAll(s, "\n", newlineFold)
		m.foldedDisplay, m.foldedOrig = folded, s
	}
	m.oneLine.SetValue(folded)
	m.oneLine.CursorEnd()
}

// resetInput clears the active widget, e.g. once its content is sent.
func (m *tui) resetInput() {
	m.foldedDisplay, m.foldedOrig = "", ""
	if m.multiline {
		m.multiLine.Reset()
		return
	}
	m.oneLine.Reset()
}

// focusInput gives the active widget the cursor.
func (m *tui) focusInput() {
	if m.multiline {
		m.multiLine.Cursor.SetMode(cursor.CursorBlink)
		m.multiLine.Focus()
		return
	}
	m.oneLine.Cursor.SetMode(cursor.CursorBlink)
	m.oneLine.Focus()
}

// blurInput takes the cursor away from both widgets — only one is ever
// showing, but a blurred textarea still draws its cursor block, which reads
// as two carets on screen once another pane takes focus.
func (m *tui) blurInput() {
	m.oneLine.Blur()
	m.oneLine.Cursor.SetMode(cursor.CursorHide)
	m.multiLine.Blur()
	m.multiLine.Cursor.SetMode(cursor.CursorHide)
}

// inputView renders whichever widget is active. The multiline box gets a
// rounded border on all four edges, in the accent colour while it has focus —
// the same convention the modal overlay's border already uses.
func (m *tui) inputView() string {
	if m.multiline {
		style := borderStyle
		if m.focus == paneInput {
			style = focusedStyle
		}
		return style.Render(m.multiLine.View())
	}
	return m.oneLine.View()
}

// setInputWidth sizes both widgets to the terminal width, so a toggle
// mid-resize never shows a stale one. The multiline box gives up two columns
// to its own left and right border edges.
func (m *tui) setInputWidth(w int) {
	// -1: the cursor renders as an extra column past Width when it sits at
	// line end, so reserving Width up to the full remaining space pushes it
	// past the terminal edge.
	m.oneLine.Width = max(w-ansi.StringWidth(m.oneLine.Prompt)-1, 1)
	m.multiLine.SetWidth(max(w-multilineBorderCols, 1))
}

// pageMultiline moves the multiline box's cursor by a page — its own content
// height — one visual row at a time through the textarea's own up/down
// handling, so the box scrolls to follow exactly as a single arrow key would.
func (m *tui) pageMultiline(up bool) {
	step := tea.KeyMsg{Type: tea.KeyDown}
	if up {
		step = tea.KeyMsg{Type: tea.KeyUp}
	}
	for i := 0; i < m.multiLine.Height(); i++ {
		m.multiLine, _ = m.multiLine.Update(step)
	}
}

// toggleMultiline flips the input between its one-row prompt and the growing
// box. Only the row count and which key sends the prompt change — the text
// and cursor cross over unharmed. Going down, a multiline draft is folded
// behind newlineFold rather than losing its line breaks; going back up
// restores them exactly if the box was not touched while folded.
func (m *tui) toggleMultiline() {
	switch {
	case m.multiline:
		orig := m.multiLine.Value()
		m.multiline = false
		m.setOneLine(orig)
	case m.foldedDisplay != "" && m.oneLine.Value() == m.foldedDisplay:
		m.multiline = true
		m.multiLine.SetValue(m.foldedOrig)
		m.multiLine.CursorEnd()
		m.foldedDisplay, m.foldedOrig = "", ""
	default:
		v := m.oneLine.Value()
		m.multiline = true
		m.multiLine.SetValue(v)
		m.multiLine.CursorEnd()
		m.foldedDisplay, m.foldedOrig = "", ""
	}
	m.focusInput()
	m.layout(m.width, m.height)
}

// inputHeight is how tall the multiline box's content stands, border
// excluded: it grows with the content between its floor and a ceiling that
// leaves the animation bar, three rows of output, the box's own border rows,
// and the help line standing. A terminal too short for all of that shrinks
// the box below its own floor rather than push those rows negative.
func inputHeight(wrapped, termHeight int) int {
	ceiling := termHeight - animHeight - outputFloor - multilineBorderRows - helpHeight
	h := max(wrapped, minMultilineHeight)
	if h > ceiling {
		h = ceiling
	}
	return max(h, 1)
}

// wrappedRows counts the rows s occupies at width, the same soft-wrap a
// textarea shows it in: each logical line takes at least one row, more if its
// display width overruns the box.
func wrappedRows(s string, width int) int {
	if width < 1 {
		width = 1
	}
	n := 0
	for _, line := range strings.Split(s, "\n") {
		n += max((ansi.StringWidth(line)+width-1)/width, 1)
	}
	return max(n, 1)
}
