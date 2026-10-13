package main

import (
	"testing"

	"github.com/memmaker/gollum/llm"
)

// A persisted effort outlives the model it was chosen for, and the config file
// it sits in is hand-editable, so it only survives for a model that offers it.
func TestValidEffort(t *testing.T) {
	for _, tc := range []struct {
		name            string
		provider, model string
		effort, want    llm.ReasoningEffort
	}{
		{"kept when the model offers it", "ollama", "qwen38-iq4:latest", llm.ReasoningEffortMax, llm.ReasoningEffortMax},
		{"dropped when it does not", "openai", "gpt-5-mini", llm.ReasoningEffortMax, ""},
		{"dropped for a model with no reasoning at all", "openai", "gpt-5-chat", llm.ReasoningEffortHigh, ""},
		{"the default passes through", "ollama", "qwen38-iq4:latest", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validEffort(tc.provider, tc.model, tc.effort); got != tc.want {
				t.Errorf("validEffort(%q, %q, %q) = %q, want %q", tc.provider, tc.model, tc.effort, got, tc.want)
			}
		})
	}
}
