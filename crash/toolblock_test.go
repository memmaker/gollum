package main

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/help"
	tea "charm.land/bubbletea/v2"

	"github.com/memmaker/gollum/llm"
)

func trimTrailingSpaces(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n")
}

// A tool call folds to one line and expands to show its full arguments and
// output, without disturbing the lines around it or losing track of a block
// added afterward.
func TestToolBlockFoldExpand(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), blockCursor: -1}
	m.transcript.WriteString("before\n\n")

	longValue := strings.Repeat("x", 200)
	longArgs := `{"path":"` + longValue + `"}`
	m.addToolCall(llm.ToolCall{ID: "1", Function: llm.ToolCallFunction{Name: "read_file", Arguments: longArgs}})
	longResult := strings.Repeat("line\n", 5) + "done"
	m.addToolResult(llm.Message{Role: llm.RoleTool, ToolCallID: "1", Content: longResult})

	// A second block, to prove toggling the first one shifts it correctly.
	m.addToolCall(llm.ToolCall{ID: "2", Function: llm.ToolCallFunction{Name: "ls", Arguments: "{}"}})
	m.addToolResult(llm.Message{Role: llm.RoleTool, ToolCallID: "2", Content: "a b c"})

	if len(m.blocks) != 2 {
		t.Fatalf("got %d blocks, want 2", len(m.blocks))
	}
	b1, b2 := m.blocks[0], m.blocks[1]
	if strings.Count(m.transcript.String(), longArgs) != 0 {
		t.Fatal("folded call already shows the full arguments")
	}
	if strings.Contains(m.transcript.String(), longResult) {
		t.Fatal("folded result already shows the full output")
	}
	b2CallBefore := b2.callLine
	if b1.resultLine != -1 || b2.resultLine != -1 {
		t.Fatal("results should stay unprinted until their block is expanded")
	}

	m.blockCursor = 0
	m.toggleSelectedBlock()

	if !b1.hasResult || b1.folded {
		t.Fatal("toggle should have expanded block 1")
	}
	got := m.transcript.String()
	// lipgloss pads each line of a multi-line Render to the block's widest
	// line, so compare trimmed lines rather than the raw substring.
	trimmed := trimTrailingSpaces(got)
	if !strings.Contains(trimmed, `"path"`) || !strings.Contains(trimmed, longValue) {
		t.Fatalf("expanded call is missing its pretty-printed arguments:\n%s", got)
	}
	if b1.callLines < 2 {
		t.Fatal("expanded call arguments should span multiple lines")
	}
	for _, line := range strings.Split(longResult, "\n") {
		if !strings.Contains(trimmed, line) {
			t.Fatalf("expanded result is missing line %q", line)
		}
	}
	if !strings.Contains(strings.Split(got, "\n")[b1.callLine], "read_file") {
		t.Fatalf("block 1's tracked callLine %d no longer points at its call", b1.callLine)
	}
	if b2.callLine <= b2CallBefore {
		t.Fatalf("block 2 did not shift down after block 1 expanded: callLine %d->%d", b2CallBefore, b2.callLine)
	}
	if b2.resultLine != -1 {
		t.Fatal("block 2's result should still be unprinted")
	}
	if !strings.Contains(strings.Split(got, "\n")[b2.callLine], "ls") {
		t.Fatalf("block 2's tracked callLine %d no longer points at its call", b2.callLine)
	}

	// Toggling back folds it again, dropping the full text.
	m.toggleSelectedBlock()
	if !b1.folded {
		t.Fatal("second toggle should have folded block 1 again")
	}
	got = m.transcript.String()
	if strings.Contains(got, longArgs) || strings.Contains(trimTrailingSpaces(got), "line\nline") {
		t.Fatal("re-folded block still shows its full text")
	}
	if !strings.HasPrefix(got, "before\n\n") {
		t.Fatal("content before the block was disturbed")
	}
	if b1.resultLine != -1 {
		t.Fatal("re-folded block's result should be unprinted again")
	}
}

// Back-to-back calls (both fired before either result lands, the common
// case for a model batching independent tool calls) must not have their
// results print out of order once they arrive — that reads as answering the
// wrong call. Results stay hidden until their own block is expanded.
func TestBackToBackCallsDeferResults(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), blockCursor: -1}
	m.addToolCall(llm.ToolCall{ID: "1", Function: llm.ToolCallFunction{Name: "first"}})
	m.addToolCall(llm.ToolCall{ID: "2", Function: llm.ToolCallFunction{Name: "second"}})
	m.addToolResult(llm.Message{Role: llm.RoleTool, ToolCallID: "1", Content: "result one"})
	m.addToolResult(llm.Message{Role: llm.RoleTool, ToolCallID: "2", Content: "result two"})

	if got := m.transcript.String(); strings.Contains(got, "result one") || strings.Contains(got, "result two") {
		t.Fatalf("results printed before either block was expanded:\n%s", got)
	}

	m.blockCursor = 0
	m.toggleSelectedBlock()
	got := m.transcript.String()
	if !strings.Contains(got, "result one") {
		t.Fatal("expanding block 1 should reveal its own result")
	}
	if strings.Contains(got, "result two") {
		t.Fatal("expanding block 1 should not reveal block 2's result")
	}
}

// The block-nav bar only makes sense while the output pane holds focus —
// left behind on an unfocused pane it reads as a second, contradicting
// selection.
func TestHighlightBlockOnlyWhenOutputFocused(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), blockCursor: 0, focus: paneOutput}
	m.addToolCall(llm.ToolCall{ID: "1", Function: llm.ToolCallFunction{Name: "a"}})

	got := m.highlightBlock(m.transcript.String())
	if !strings.Contains(got, "▎") {
		t.Fatal("output pane focused: bar should be drawn")
	}

	m.focus = paneSidebar
	got = m.highlightBlock(m.transcript.String())
	if strings.Contains(got, "▎") {
		t.Fatal("output pane unfocused: bar should be gone")
	}
}

func TestSelectBlockWraps(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), blockCursor: -1}
	m.addToolCall(llm.ToolCall{ID: "1", Function: llm.ToolCallFunction{Name: "a"}})
	m.addToolCall(llm.ToolCall{ID: "2", Function: llm.ToolCallFunction{Name: "b"}})

	m.selectBlock(1)
	if m.blockCursor != 0 {
		t.Fatalf("first down from unselected: got cursor %d, want 0", m.blockCursor)
	}
	m.selectBlock(1)
	if m.blockCursor != 1 {
		t.Fatalf("second down: got cursor %d, want 1", m.blockCursor)
	}
	m.selectBlock(1)
	if m.blockCursor != 1 {
		t.Fatalf("down at the last block should stay put: got %d, want 1", m.blockCursor)
	}
}

// Scrolling and block selection share the output pane, as they do in crush:
// the arrows move one line and only carry the cursor along once its block has
// left the screen, shift+arrows step block to block, and a page lands the
// cursor on what the page brought into view.
func TestOutputScrollAndBlockSelection(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New(), blockCursor: -1}
	m.layout(100, 20)
	m.focus = paneOutput

	// Two tool calls, a screenful of filler between them.
	m.addToolCall(llm.ToolCall{ID: "1", Function: llm.ToolCallFunction{Name: "a"}})
	m.transcript.WriteString(strings.Repeat("filler\n", 60))
	m.addToolCall(llm.ToolCall{ID: "2", Function: llm.ToolCallFunction{Name: "b"}})
	m.setOutput(m.transcript.String())
	m.out.GotoTop()

	// An arrow scrolls one line and leaves the cursor where it was.
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if got := m.out.YOffset(); got != 1 {
		t.Fatalf("down scrolled to line %d, want one line", got)
	}
	if m.blockCursor != -1 {
		t.Fatalf("a plain scroll moved the cursor to %d", m.blockCursor)
	}

	// shift+arrow steps block to block.
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModShift})
	if m.blockCursor != 0 {
		t.Fatalf("shift+down landed on block %d, want 0", m.blockCursor)
	}

	// Scrolling the selected block off the top carries the cursor along.
	for range m.out.Height() + 2 {
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if m.blockCursor != 1 {
		t.Fatalf("the cursor was left behind on block %d, want 1", m.blockCursor)
	}

	// A page lands the cursor on a block the page put in front of you.
	m.out.GotoTop()
	m.blockCursor = 1
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.blockCursor != 0 {
		t.Fatalf("pgup left the cursor on block %d, want the one in view (0)", m.blockCursor)
	}
}
