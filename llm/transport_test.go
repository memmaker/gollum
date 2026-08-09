package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenAIServiceTier(t *testing.T) {
	cases := []struct {
		name    string
		tier    ServiceTier
		allowed bool
		want    bool // payload contains "service_tier":"flex"
	}{
		{"openai flex", ServiceTierFlex, true, true},
		{"openai unset", "", true, false},
		{"compatible backend flex", ServiceTierFlex, false, false},
	}
	for _, c := range cases {
		tr := &openaiTransport{allowServiceTier: c.allowed}
		payload, err := json.Marshal(openaiReq{
			Model:       "gpt-5-mini",
			ServiceTier: tr.serviceTier(c.tier),
		})
		if err != nil {
			t.Fatalf("%s: marshal: %v", c.name, err)
		}
		if got := strings.Contains(string(payload), `"service_tier":"flex"`); got != c.want {
			t.Errorf("%s: service_tier present = %v, want %v (payload %s)", c.name, got, c.want, payload)
		}
		if !c.want && strings.Contains(string(payload), "service_tier") {
			t.Errorf("%s: service_tier key should be absent entirely, got %s", c.name, payload)
		}
	}
}

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
