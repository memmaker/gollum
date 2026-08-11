package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/memmaker/gollum/llm"
)

// ToolConfig is one built-in tool's state: whether it is enabled.
type ToolConfig struct {
	Name    string
	Enabled bool
}

// registerTools installs the harness tools on a session, but only ones the user
// has enabled. The llm library ships no tools of its own — anything that talks
// to a terminal lives here.
func registerTools(s *llm.Session, toolConfigs []ToolConfig) {
	enabledByName := make(map[string]bool)
	for _, c := range toolConfigs {
		enabledByName[c.Name] = c.Enabled
	}

	if enabledByName["user_choice"] {
		s.AddTool(llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "user_choice",
				Description: "Ask the user to pick one of several options in the terminal. Returns the chosen option, or an error if the user cancels.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"question": map[string]any{
							"type":        "string",
							"description": "Question shown above the options",
						},
						"options": map[string]any{
							"type":        "array",
							"items":       map[string]any{"type": "string"},
							"description": "The options to choose from",
						},
					},
					"required": []string{"question", "options"},
				},
			},
		}, execUserChoice)
	}

	if enabledByName["bash"] {
		s.AddTool(llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "bash",
				Description: "Execute a shell command in the terminal. Returns the stdout output.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{
							"type":        "string",
							"description": "The shell command to execute",
						},
					},
					"required": []string{"command"},
				},
			},
		}, execBash)
	}
}

type userChoiceArgs struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
}

func execUserChoice(_ context.Context, call llm.ToolCall) (string, error) {
	var args userChoiceArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "", fmt.Errorf("user_choice: parse args: %w", err)
	}
	idx := ask(args.Question, args.Options)
	if idx < 0 {
		return "", fmt.Errorf("user_choice: cancelled by user")
	}
	return args.Options[idx], nil
}

type bashArgs struct {
	Command string `json:"command"`
}

func execBash(_ context.Context, call llm.ToolCall) (string, error) {
	var args bashArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "", fmt.Errorf("bash: parse args: %w", err)
	}
	out, err := exec.Command("sh", "-c", args.Command).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("error: %v\noutput: %s", err, string(out)), nil
	}
	return string(out), nil
}

// ask puts a choice overlay on screen from a tool's goroutine and blocks until
// the user answers. Returns -1 if they cancel.
func ask(question string, options []string) int {
	if prog == nil {
		return -1
	}
	reply := make(chan int, 1)
	prog.Send(askMsg{question: question, options: options, reply: reply})
	return <-reply
}
