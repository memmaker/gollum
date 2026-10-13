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
		model   string
		want    bool // payload contains "service_tier":"flex"
	}{
		{"openai flex", ServiceTierFlex, true, "gpt-5-mini", true},
		{"openai unset", "", true, "gpt-5-mini", false},
		{"compatible backend flex", ServiceTierFlex, false, "gpt-5-mini", false},
		{"omniroute to openai", ServiceTierFlex, true, "openai/gpt-5-mini", true},
		{"omniroute to gemini", ServiceTierFlex, true, "gemini/gemini-3.5-flash", false},
	}
	for _, c := range cases {
		tr := &openaiTransport{allowServiceTier: c.allowed}
		payload, err := json.Marshal(openaiReq{
			Model:       c.model,
			ServiceTier: tr.serviceTier(c.tier, c.model),
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
		{ReasoningEffortNone, "gpt-5.6-luna", "none"},
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

func TestToolChoice(t *testing.T) {
	oneTool := []Tool{{Type: "function", Function: ToolFunction{Name: "roll"}}}
	cases := []struct {
		name   string
		choice ToolChoice
		tools  []Tool
		want   bool // payload contains "tool_choice":"required"
	}{
		{"required with tools", ToolChoiceRequired, oneTool, true},
		{"required without tools", ToolChoiceRequired, nil, false},
		{"unset with tools", "", oneTool, false},
	}
	for _, c := range cases {
		tr := &openaiTransport{}
		body := tr.body(Request{Model: "gpt-5-mini", Tools: c.tools, ToolChoice: c.choice}, false)
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("%s: marshal: %v", c.name, err)
		}
		if got := strings.Contains(string(payload), `"tool_choice":"required"`); got != c.want {
			t.Errorf("%s: tool_choice present = %v, want %v (payload %s)", c.name, got, c.want, payload)
		}
	}
}
