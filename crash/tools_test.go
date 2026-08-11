package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/help"
	"github.com/memmaker/gollum/llm"
)

// Tool toggle persists state and updates the session's active tools.
func TestToolMenuToggles(t *testing.T) {
	t.Chdir(t.TempDir())
	const cfg = "provider: openai"
	if err := os.WriteFile("llm_model.rec", []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}

	toolCfgs := []ToolConfig{
		{Name: "user_choice", Enabled: true},
		{Name: "bash", Enabled: true},
	}

	sess := llm.NewSession(nil, "")
	registerTools(sess, toolCfgs)

	m := &tui{
		sess:        sess,
		oneLine:     newOneLineInput(),
		multiLine:   newMultiLineInput(),
		help:        help.New(),
		toolConfigs: toolCfgs,
	}
	m.layout(100, 30)

	// Both tools enabled initially
	if m.toolConfigs[0].Name != "user_choice" || !m.toolConfigs[0].Enabled {
		t.Fatal("user_choice should start enabled")
	}
	if m.toolConfigs[1].Name != "bash" || !m.toolConfigs[1].Enabled {
		t.Fatal("bash should start enabled")
	}

	// Toggle bash off
	m.toggleTool("bash")
	if m.toolConfigs[1].Enabled {
		t.Fatal("bash still enabled after toggle")
	}

	// State persisted to config file
	loaded, err := LoadToolConfigs()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tc := range loaded {
		if tc.Name == "bash" {
			found = true
			if tc.Enabled {
				t.Fatal("bash enabled in persisted config after toggle")
			}
		}
	}
	if !found {
		t.Fatal("bash not found in persisted config")
	}

	// Toggle back on
	m.toggleTool("bash")
	if !m.toolConfigs[1].Enabled {
		t.Fatal("bash disabled after toggling back on")
	}

	// Fresh load gets the new state
	loaded, err = LoadToolConfigs()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range loaded {
		if tc.Name == "bash" && !tc.Enabled {
			t.Fatal("bash disabled in fresh load after toggle back on")
		}
	}
}

// Tools menu stays open across toggles and returns to main menu on cancel.
func TestToolsMenuStaysOpenAcrossToggles(t *testing.T) {
	t.Chdir(t.TempDir())
	const cfg = "provider: openai"
	if err := os.WriteFile("llm_model.rec", []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}

	toolCfgs := []ToolConfig{
		{Name: "user_choice", Enabled: true},
		{Name: "bash", Enabled: true},
	}

	sess := llm.NewSession(nil, "")
	registerTools(sess, toolCfgs)

	m := &tui{
		sess:        sess,
		oneLine:     newOneLineInput(),
		multiLine:   newMultiLineInput(),
		help:        help.New(),
		toolConfigs: toolCfgs,
	}
	m.layout(100, 30)

	m.openToolsMenu()
	if m.choice == nil {
		t.Fatal("openToolsMenu did not open a choice overlay")
	}

	firstLabel := m.choice.VisibleItems()[0].(choiceItem).text
	if !strings.Contains(firstLabel, "user_choice") || !strings.Contains(firstLabel, "on") {
		t.Fatalf("first item = %q, want user_choice: on", firstLabel)
	}

	if strings.Contains(m.choice.VisibleItems()[1].(choiceItem).text, "off") {
		t.Fatal("bash should start as on")
	}

	// Toggle bash
	m.closeChoice(1)
	if m.choice == nil {
		t.Fatal("tools menu closed after toggle, want it to stay open")
	}

	// Verify bash state changed in the label
	if m.toolConfigs[1].Enabled {
		t.Fatal("bash should be disabled now")
	}

	secondLabel := m.choice.VisibleItems()[1].(choiceItem).text
	if !strings.Contains(secondLabel, "bash") || !strings.Contains(secondLabel, "off") {
		t.Fatalf("second item = %q, want bash: off", secondLabel)
	}

	// Cancel closes menu back to main menu
	m.closeChoice(-1)
	if m.choice == nil {
		t.Fatal("main menu should reopen after tools menu cancelled")
	}

	firstMainLabel := m.choice.VisibleItems()[0].(choiceItem).text
	if !strings.Contains(firstMainLabel, "Send tools") {
		t.Fatalf("back to main menu, first item = %q, want Send tools", firstMainLabel)
	}
}

// Bash tool executes shell commands and returns output.
func TestBashToolExecution(t *testing.T) {
	ctx := context.Background()
	call := llm.ToolCall{
		Function: llm.ToolCallFunction{
			Name:      "bash",
			Arguments: `{"command": "echo hello"}`,
		},
	}

	result, err := execBash(ctx, call)
	if err != nil {
		t.Fatalf("bash execution failed: %v", err)
	}
	if !strings.Contains(result, "hello") {
		t.Fatalf("bash output = %q, want it to contain hello", result)
	}
}

// Bash tool returns error output on command failure.
func TestBashToolError(t *testing.T) {
	ctx := context.Background()
	call := llm.ToolCall{
		Function: llm.ToolCallFunction{
			Name:      "bash",
			Arguments: `{"command": "exit 42"}`,
		},
	}

	result, err := execBash(ctx, call)
	if err != nil {
		t.Fatalf("bash should not return go error for cmd failure: %v", err)
	}
	if !strings.Contains(result, "error:") {
		t.Fatalf("bash result = %q, want it to indicate an error", result)
	}
}
