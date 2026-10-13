package main

import (
	"os"
	"strings"
	"testing"

	"github.com/memmaker/gollum/llm"
)

// Everything a session appends lands in the log as it happens, and reading it
// back gives the conversation without the system prompt.
func TestSessionLogRoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())

	sess := llm.NewSession(nil, "you are a test")
	log := openSessionLog()
	sess.Log = log.write

	sess.Restore([]llm.Message{
		{Role: llm.RoleUser, Content: "hi\nthere"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
			ID: "call_1", Type: "function",
			Function: llm.ToolCallFunction{Name: "bash", Arguments: `{"cmd":"ls"}`},
		}}},
		{Role: llm.RoleTool, ToolCallID: "call_1", Name: "bash", Content: "a\nb"},
		{Role: llm.RoleAssistant, Content: "hello"},
	})

	got := loadSession()
	if len(got) != 4 {
		t.Fatalf("restored %d messages, want 4: %+v", len(got), got)
	}
	if got[0].Content != "hi\nthere" {
		t.Errorf("multi-line content: %q", got[0].Content)
	}
	if len(got[1].ToolCalls) != 1 || got[1].ToolCalls[0].Function.Arguments != `{"cmd":"ls"}` {
		t.Errorf("tool call: %+v", got[1].ToolCalls)
	}
	if got[2].ToolCallID != "call_1" || got[2].Name != "bash" || got[2].Content != "a\nb" {
		t.Errorf("tool result: %+v", got[2])
	}
	if got[3].Content != "hello" {
		t.Errorf("answer: %q", got[3].Content)
	}

	b, err := os.ReadFile(sessionFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "you are a test") {
		t.Error("system prompt was written to the log")
	}

	// A clear replaces the history, so what came before it stops being read
	// back — but the file keeps it.
	before := len(b)
	sess.Clear()
	if got := loadSession(); got != nil {
		t.Fatalf("cleared session still restores %+v", got)
	}
	if b, _ := os.ReadFile(sessionFile); len(b) <= before {
		t.Error("clear should append a reset event, not truncate")
	}
	if err := log.close(); err != nil {
		t.Fatal(err)
	}
}

// A log written by one run is continued, not replaced, by the next.
func TestSessionLogAppendsAcrossRuns(t *testing.T) {
	t.Chdir(t.TempDir())

	first := openSessionLog()
	first.write([]llm.Message{{Role: llm.RoleUser, Content: "one"}}, false)
	first.close()

	second := openSessionLog()
	second.write([]llm.Message{{Role: llm.RoleUser, Content: "two"}}, false)
	second.close()

	got := loadSession()
	if len(got) != 2 || got[0].Content != "one" || got[1].Content != "two" {
		t.Fatalf("round trip across runs: %+v", got)
	}
}

// A missing log is a fresh start, not an error.
func TestLoadSessionMissingFile(t *testing.T) {
	t.Chdir(t.TempDir())
	if got := loadSession(); got != nil {
		t.Fatalf("missing file should load as nothing, got %+v", got)
	}
}
