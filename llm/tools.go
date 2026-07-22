package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
)

func builtinTools() []Tool {
	return []Tool{
		{
			Type: "function",
			Function: ToolFunction{
				Name:        "random",
				Description: "Generate a random integer between min and max (inclusive).",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"min": map[string]any{
							"type":        "integer",
							"description": "Minimum value (default 0)",
						},
						"max": map[string]any{
							"type":        "integer",
							"description": "Maximum value",
						},
					},
					"required": []string{"max"},
				},
			},
		},
	}
}

func builtinExecutors() map[string]ToolExecutor {
	return map[string]ToolExecutor{
		"random": execRandom,
	}
}

type randomArgs struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

func execRandom(_ context.Context, call ToolCall) (string, error) {
	var args randomArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "", fmt.Errorf("random: parse args: %w", err)
	}
	if args.Max < args.Min {
		return "", fmt.Errorf("random: max (%d) must be >= min (%d)", args.Max, args.Min)
	}
	span := args.Max - args.Min + 1
	n := rand.Intn(span) + args.Min
	return fmt.Sprintf("%d", n), nil
}
