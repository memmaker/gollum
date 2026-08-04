package llm

import "testing"

func TestOpenAIReasoningEffort(t *testing.T) {
	cases := []struct {
		effort ReasoningEffort
		model  string
		want   string
	}{
		{ReasoningEffortNone, "gpt-5-mini", "minimal"},
		{ReasoningEffortNone, "gpt-4o", "none"},
		{ReasoningEffortNone, "gpt-5.3-chat-latest", ""},
		{ReasoningEffortHigh, "gpt-5-chat-latest", ""},
		{ReasoningEffortHigh, "o3", "high"},
	}
	for _, c := range cases {
		if got := openaiReasoningEffort(c.effort, c.model); got != c.want {
			t.Errorf("openaiReasoningEffort(%q, %q) = %q, want %q", c.effort, c.model, got, c.want)
		}
	}
}
