package main

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/memmaker/gollum/llm"
)

// toolBlock is one tool call plus its result (once it arrives), tracked so
// the pair can be folded (the default, one line each) or expanded in place —
// full arguments, full output — without disturbing anything around it in the
// transcript. Stolen from Crush's tool-call block UI.
type toolBlock struct {
	call      llm.ToolCall
	result    llm.Message
	hasResult bool
	folded    bool

	// Positions are 0-indexed lines within m.transcript's current text, kept
	// up to date by spliceLine as earlier blocks grow or shrink.
	callLine, callLines     int
	resultLine, resultLines int // resultLine is -1 until hasResult
}

// blockFull reports whether b should render with full arguments/output:
// either /debug forces every block open, or this one was expanded by hand.
func (m *tui) blockFull(b *toolBlock) bool {
	if b == nil {
		return m.debug
	}
	return m.debug || !b.folded
}

func (m *tui) renderToolCall(tc llm.ToolCall, full bool) string {
	args := strings.TrimSpace(tc.Function.Arguments)
	if !full {
		args = ellipsis(args, max(m.out.Width-ansi.StringWidth(tc.Function.Name)-6, 10))
	}
	return toolStyle.Render("⚒ "+tc.Function.Name) + " " + dim.Render(args)
}

func (m *tui) renderToolResult(msg llm.Message, full bool) string {
	body := strings.TrimSpace(msg.Content)
	style := dim
	if strings.HasPrefix(body, "error:") {
		style = errStyle
	}
	if !full {
		body = ellipsis(body, max(m.out.Width-6, 10))
	}
	return style.Render("  ↳ " + body)
}

// addToolCall appends a folded call line to the transcript and starts
// tracking it as a block.
func (m *tui) addToolCall(tc llm.ToolCall) {
	b := &toolBlock{call: tc, folded: true, resultLine: -1}
	b.callLine = strings.Count(m.transcript.String(), "\n")
	text := m.renderToolCall(tc, m.blockFull(b))
	b.callLines = strings.Count(text, "\n") + 1
	m.transcript.WriteString(text + "\n")
	m.blocks = append(m.blocks, b)
}

// addToolResult appends a result line and, if it can be matched back to the
// call that produced it, folds it into that block.
func (m *tui) addToolResult(msg llm.Message) {
	b := m.pendingBlock(msg.ToolCallID)
	text := m.renderToolResult(msg, m.blockFull(b))
	if b != nil {
		b.result, b.hasResult = msg, true
		b.resultLine = strings.Count(m.transcript.String(), "\n")
		b.resultLines = strings.Count(text, "\n") + 1
	}
	m.transcript.WriteString(text + "\n\n")
}

// pendingBlock finds the most recent call still waiting on a result, matched
// by tool_call_id where the backend sends one, else by arrival order.
func (m *tui) pendingBlock(id string) *toolBlock {
	for i := len(m.blocks) - 1; i >= 0; i-- {
		b := m.blocks[i]
		if b.hasResult {
			continue
		}
		if id == "" || b.call.ID == id {
			return b
		}
	}
	return nil
}

// toggleSelectedBlock flips the fold state of the block currently under the
// block-nav cursor and re-renders it in place.
func (m *tui) toggleSelectedBlock() {
	if m.blockCursor < 0 || m.blockCursor >= len(m.blocks) {
		return
	}
	b := m.blocks[m.blockCursor]
	b.folded = !b.folded

	newCall := m.renderToolCall(b.call, m.blockFull(b))
	m.spliceLine(b.callLine, b.callLines, newCall)
	b.callLines = strings.Count(newCall, "\n") + 1

	if b.hasResult {
		newResult := m.renderToolResult(b.result, m.blockFull(b))
		m.spliceLine(b.resultLine, b.resultLines, newResult)
		b.resultLines = strings.Count(newResult, "\n") + 1
	}
	m.redraw()
}

// spliceLine replaces the oldCount lines of m.transcript starting at line
// index at with newText, which may itself expand into more or fewer lines,
// then shifts every block position recorded after the edit to match.
func (m *tui) spliceLine(at, oldCount int, newText string) {
	lines := strings.Split(m.transcript.String(), "\n")
	if at < 0 || at+oldCount > len(lines) {
		return
	}
	newLines := strings.Split(newText, "\n")
	rebuilt := make([]string, 0, len(lines)-oldCount+len(newLines))
	rebuilt = append(rebuilt, lines[:at]...)
	rebuilt = append(rebuilt, newLines...)
	rebuilt = append(rebuilt, lines[at+oldCount:]...)
	m.transcript.Reset()
	m.transcript.WriteString(strings.Join(rebuilt, "\n"))

	if delta := len(newLines) - oldCount; delta != 0 {
		for _, blk := range m.blocks {
			if blk.callLine > at {
				blk.callLine += delta
			}
			if blk.hasResult && blk.resultLine > at {
				blk.resultLine += delta
			}
		}
	}
}

// selectBlock moves the block-nav cursor by dir (+1/-1), wrapping in from
// whichever end makes sense, and scrolls it into view.
func (m *tui) selectBlock(dir int) {
	if len(m.blocks) == 0 {
		return
	}
	if m.blockCursor < 0 {
		if dir > 0 {
			m.blockCursor = 0
		} else {
			m.blockCursor = len(m.blocks) - 1
		}
	} else {
		m.blockCursor = min(max(m.blockCursor+dir, 0), len(m.blocks)-1)
	}
	m.scrollToBlock(m.blocks[m.blockCursor])
	m.redraw()
}

// scrollToBlock nudges the output viewport just enough to bring b fully into
// view, the way jumping to a search match would.
func (m *tui) scrollToBlock(b *toolBlock) {
	top := b.callLine
	bottom := b.callLine + b.callLines - 1
	if b.hasResult {
		bottom = b.resultLine + b.resultLines - 1
	}
	switch {
	case top < m.out.YOffset:
		m.out.SetYOffset(top)
	case bottom >= m.out.YOffset+m.out.Height:
		m.out.SetYOffset(bottom - m.out.Height + 1)
	}
}

// handleBlockKey lets the output pane's tool blocks be picked and
// folded/expanded without taking over the viewport's paging keys — pgup,
// pgdown, g and G still scroll. Only kicks in once there is a block to pick;
// otherwise up/down are left to scroll plain text as before.
func (m *tui) handleBlockKey(msg string) bool {
	if len(m.blocks) == 0 {
		return false
	}
	switch msg {
	case "up", "k":
		m.selectBlock(-1)
	case "down", "j":
		m.selectBlock(1)
	case "enter":
		m.toggleSelectedBlock()
	default:
		return false
	}
	return true
}

// highlightBlock draws the green left bar Crush uses to mark the block-nav
// cursor's tool call, over both its call and result lines.
func (m *tui) highlightBlock(content string) string {
	if m.blockCursor < 0 || m.blockCursor >= len(m.blocks) {
		return content
	}
	b := m.blocks[m.blockCursor]
	lines := strings.Split(content, "\n")
	mark := func(from, n int) {
		for i := from; i < from+n && i >= 0 && i < len(lines); i++ {
			lines[i] = blockBarStyle.Render("▎") + lines[i]
		}
	}
	mark(b.callLine, b.callLines)
	if b.hasResult {
		mark(b.resultLine, b.resultLines)
	}
	return strings.Join(lines, "\n")
}
