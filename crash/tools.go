package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/memmaker/gollum/llm"
)

// registerTools installs the harness tools on a session. The llm library ships
// no tools of its own — anything that talks to a terminal lives here.
func registerTools(s *llm.Session) {
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
