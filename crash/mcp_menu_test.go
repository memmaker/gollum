package main

import (
	"context"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"charm.land/bubbles/v2/help"

	"github.com/memmaker/gollum/llm"
)

// nopWriteCloser discards writes, standing in for a real subprocess's stdin
// in tests that never actually send a request through it.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// The MCPs submenu stays open across toggles like the main menu's own
// toggle entries, persists the change, and drops the tool from both the
// running client map and the "Tools" listing. Cancelling it returns to the
// main menu.
func TestMCPMenuTogglesAndReturnsToMainMenu(t *testing.T) {
	t.Chdir(t.TempDir())
	const cfg = "provider: openai\n\nmcp: fake\ncommand: does-not-matter\nargs: mcp\n"
	if err := os.WriteFile("llm_model.rec", []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}

	sess := llm.NewSession(nil, "")
	client := newMCPClient("fake", nopWriteCloser{Writer: io.Discard}, strings.NewReader(""), nil)
	client.tools = []mcpToolDef{{Name: "fake_tool", Description: "does a thing"}}
	registerMCPTools(sess, client, nil)

	m := &tui{
		sess:       sess,
		oneLine:    newOneLineInput(),
		multiLine:  newMultiLineInput(),
		help:       help.New(),
		mcpConfigs: []MCPConfig{{Name: "fake", Command: "does-not-matter", Args: []string{"mcp"}, Enabled: true}},
		mcpClients: map[string]*mcpClient{"fake": client},
	}
	m.layout(100, 30)

	if !slices.Contains(m.toolMenuNames(), "fake_tool") {
		t.Fatal("tool listing is missing fake_tool before toggling")
	}

	m.openMenu()
	m.closeChoice(menuEntry(t, m, "MCPs"))
	if m.choice == nil {
		t.Fatal("MCPs entry did not open a submenu")
	}
	if got := m.choice.VisibleItems()[0].(choiceItem).text; got != "fake: on" {
		t.Fatalf("submenu shows %q, want \"fake: on\"", got)
	}

	m.closeChoice(0) // toggle fake off
	if m.choice == nil {
		t.Fatal("MCP submenu closed after a toggle, want it to stay open")
	}
	if got := m.choice.VisibleItems()[0].(choiceItem).text; got != "fake: off" {
		t.Fatalf("submenu shows %q, want \"fake: off\"", got)
	}
	if m.mcpConfigs[0].Enabled {
		t.Fatal("config still enabled after toggling off")
	}
	if _, ok := m.mcpClients["fake"]; ok {
		t.Fatal("client not removed after toggling off")
	}
	if slices.Contains(m.toolMenuNames(), "fake_tool") {
		t.Fatal("tool listing still shows fake_tool after its MCP was disabled")
	}

	// The toggle persisted: a fresh load sees it off too.
	mcps, err := LoadMCPConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(mcps) != 1 || mcps[0].Enabled {
		t.Fatalf("persisted mcp config = %+v, want enabled=false", mcps)
	}

	m.closeChoice(-1) // cancel the submenu
	if m.choice == nil {
		t.Fatal("main menu did not come back after the MCP submenu was cancelled")
	}
	if got := m.choice.VisibleItems()[0].(choiceItem).text; !strings.HasPrefix(got, "Send tools") {
		t.Fatalf("came back to %q, want the main menu", got)
	}
}

// A tool call discovering its MCP's connection is gone must disable that MCP
// and tell the user, not just fail the one call — otherwise the menu and the
// "Tools" listing keep claiming a dead server is still running.
func TestMCPDeathDisablesAndNotes(t *testing.T) {
	t.Chdir(t.TempDir())

	sess := llm.NewSession(nil, "")
	client := newMCPClient("fake", nopWriteCloser{Writer: io.Discard}, strings.NewReader(""), nil)
	client.tools = []mcpToolDef{{Name: "fake_tool"}}
	registerMCPTools(sess, client, nil)

	m := &tui{
		sess:       sess,
		oneLine:    newOneLineInput(),
		multiLine:  newMultiLineInput(),
		help:       help.New(),
		mcpConfigs: []MCPConfig{{Name: "fake", Enabled: true}},
		mcpClients: map[string]*mcpClient{"fake": client},
	}
	m.layout(100, 30)

	// A dead connection: the executor calling isDead() sees the read loop
	// already ended, same as a real process that exited or broke its pipe.
	client.close()
	<-client.dead

	m.Update(mcpDeadMsg{name: "fake", err: context.DeadlineExceeded})

	if m.mcpConfigs[0].Enabled {
		t.Fatal("mcp still marked enabled after its connection died")
	}
	if _, ok := m.mcpClients["fake"]; ok {
		t.Fatal("dead client not removed from the running set")
	}
	if !strings.Contains(m.note, "fake") {
		t.Fatalf("note = %q, want it to mention the dead mcp", m.note)
	}

	// A death report for an mcp that is no longer tracked (already handled,
	// or never existed) must be a no-op, not a panic on a missing config.
	m.Update(mcpDeadMsg{name: "fake", err: context.DeadlineExceeded})
	m.Update(mcpDeadMsg{name: "never-registered", err: context.DeadlineExceeded})
}
