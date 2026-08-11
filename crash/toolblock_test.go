package main

import (
	"strings"
	"testing"

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
	b2CallBefore, b2ResultBefore := b2.callLine, b2.resultLine

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
	if b2.callLine <= b2CallBefore || b2.resultLine <= b2ResultBefore {
		t.Fatalf("block 2 did not shift down after block 1 expanded: callLine %d->%d, resultLine %d->%d",
			b2CallBefore, b2.callLine, b2ResultBefore, b2.resultLine)
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
