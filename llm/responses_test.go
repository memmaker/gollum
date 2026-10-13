package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// serve stands in for the Responses endpoint, handing back a canned body and
// keeping the request it was sent so a test can assert on the wire format.
func serve(t *testing.T, reply string) (*responsesProvider, *map[string]any) {
	t.Helper()
	sent := map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Errorf("server could not decode the request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(reply))
	}))
	t.Cleanup(server.Close)

	p := newResponsesProvider(Config{APIKey: "k", Model: "gpt-5.6-luna", BaseURL: server.URL, Timeout: 5 * time.Second}, "", true)
	return p, &sent
}

func TestResponsesFlattensAConversationIntoInputItems(t *testing.T) {
	p, sent := serve(t, `{"status":"completed","output":[]}`)

	_, err := p.Chat(context.Background(), Request{Messages: []Message{
		{Role: RoleSystem, Content: "you keep books"},
		{Role: RoleUser, Content: "take the torch"},
		{Role: RoleAssistant, Content: "on it", ToolCalls: []ToolCall{
			{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "move", Arguments: `{"id":"itm001"}`}},
			{ID: "call_2", Type: "function", Function: ToolCallFunction{Name: "done"}},
		}},
		{Role: RoleTool, ToolCallID: "call_1", Content: "Moved."},
	}})
	if err != nil {
		t.Fatal(err)
	}

	input, _ := (*sent)["input"].([]any)
	if len(input) != 6 {
		t.Fatalf("got %d input items, want 6:\n%s", len(input), pretty(*sent))
	}
	// One Message becomes several items: the assistant turn above both said
	// something and called two tools, so it is three of them.
	want := []map[string]string{
		{"role": "system", "content": "you keep books"},
		{"role": "user", "content": "take the torch"},
		{"role": "assistant", "content": "on it"},
		{"type": "function_call", "call_id": "call_1", "name": "move", "arguments": `{"id":"itm001"}`},
		{"type": "function_call", "call_id": "call_2", "name": "done", "arguments": "{}"},
		{"type": "function_call_output", "call_id": "call_1", "output": "Moved."},
	}
	for i, fields := range want {
		item, _ := input[i].(map[string]any)
		for k, v := range fields {
			if got, _ := item[k].(string); got != v {
				t.Errorf("input[%d].%s = %q, want %q", i, k, got, v)
			}
		}
		// A union member must not carry another member's keys.
		if _, isMessage := fields["role"]; isMessage {
			if _, leaked := item["call_id"]; leaked {
				t.Errorf("input[%d] is a message but carries call_id", i)
			}
		}
	}
}

func TestResponsesGivesAnEmptyArgumentListParseableJSON(t *testing.T) {
	p, sent := serve(t, `{"status":"completed","output":[]}`)
	_, err := p.Chat(context.Background(), Request{Messages: []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c", Function: ToolCallFunction{Name: "done"}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	item, _ := (*sent)["input"].([]any)[0].(map[string]any)
	if item["arguments"] != "{}" {
		t.Fatalf("empty arguments were sent as %q, want %q", item["arguments"], "{}")
	}
}

func TestResponsesSendsFlatToolsAndTheReasoningEnvelope(t *testing.T) {
	p, sent := serve(t, `{"status":"completed","output":[]}`)

	_, err := p.Chat(context.Background(), Request{
		Messages:        []Message{{Role: RoleUser, Content: "hi"}},
		ReasoningEffort: ReasoningEffortLow,
		ServiceTier:     ServiceTierFlex,
		ToolChoice:      ToolChoiceRequired,
		Tools: []Tool{{Type: "function", Function: ToolFunction{
			Name: "move", Description: "moves a thing",
			Parameters: map[string]any{"type": "object"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	tools, _ := (*sent)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("got %d tools:\n%s", len(tools), pretty(*sent))
	}
	tool, _ := tools[0].(map[string]any)
	// Flat, not nested under "function" the way chat/completions wants it.
	if tool["name"] != "move" || tool["type"] != "function" {
		t.Errorf("tool = %v, want a flat function tool", tool)
	}
	if _, nested := tool["function"]; nested {
		t.Error("the tool is still nested under a \"function\" key")
	}
	if tool["strict"] != false {
		t.Errorf("strict = %v, want an explicit false", tool["strict"])
	}

	reasoning, _ := (*sent)["reasoning"].(map[string]any)
	if reasoning["effort"] != "low" {
		t.Errorf("reasoning = %v, want {effort: low}", reasoning)
	}
	if _, flat := (*sent)["reasoning_effort"]; flat {
		t.Error("reasoning_effort was sent as well as reasoning")
	}
	if (*sent)["tool_choice"] != "required" {
		t.Errorf("tool_choice = %v", (*sent)["tool_choice"])
	}
	if (*sent)["service_tier"] != "flex" {
		t.Errorf("service_tier = %v", (*sent)["service_tier"])
	}
	// Not storing the caller's prompts on the provider's side is a decision
	// this package makes explicitly, so the field has to be on the wire.
	if (*sent)["store"] != false {
		t.Errorf("store = %v, want an explicit false", (*sent)["store"])
	}
}

// The whole reason this transport exists: an effort together with tools.
func TestResponsesSendsAnEffortAlongsideTools(t *testing.T) {
	p, sent := serve(t, `{"status":"completed","output":[]}`)
	_, err := p.Chat(context.Background(), Request{
		Messages:        []Message{{Role: RoleUser, Content: "hi"}},
		ReasoningEffort: ReasoningEffortMedium,
		Tools:           []Tool{{Type: "function", Function: ToolFunction{Name: "move"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reasoning, _ := (*sent)["reasoning"].(map[string]any)
	if reasoning["effort"] != "medium" {
		t.Fatalf("an effort was dropped when tools were present: %s", pretty(*sent))
	}
}

func TestResponsesReadsTextToolCallsAndUsage(t *testing.T) {
	p, _ := serve(t, `{
	  "status":"completed",
	  "output":[
	    {"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]},
	    {"type":"message","content":[{"type":"output_text","text":"Done, "},{"type":"output_text","text":"twice."}]},
	    {"type":"function_call","call_id":"call_a","name":"move","arguments":"{\"id\":\"itm001\"}"},
	    {"type":"function_call","call_id":"call_b","name":"done","arguments":"{}"}
	  ],
	  "usage":{"input_tokens":11,"output_tokens":5,"total_tokens":16,"output_tokens_details":{"reasoning_tokens":3}}
	}`)

	res, err := p.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.Content != "Done, twice." {
		t.Errorf("content = %q", res.Message.Content)
	}
	if res.Message.Reasoning != "thinking" {
		t.Errorf("reasoning = %q", res.Message.Reasoning)
	}
	if len(res.Message.ToolCalls) != 2 {
		t.Fatalf("got %d tool calls, want 2", len(res.Message.ToolCalls))
	}
	// Order is the model's, and callers depend on it.
	if res.Message.ToolCalls[0].ID != "call_a" || res.Message.ToolCalls[1].ID != "call_b" {
		t.Errorf("tool calls came back out of order: %+v", res.Message.ToolCalls)
	}
	if res.Message.ToolCalls[0].Function.Name != "move" {
		t.Errorf("tool call name = %q", res.Message.ToolCalls[0].Function.Name)
	}
	want := Usage{InputTokens: 11, OutputTokens: 5, TotalTokens: 16, ReasoningTokens: 3}
	if res.Usage != want {
		t.Errorf("usage = %+v, want %+v", res.Usage, want)
	}
}

// A Responses call can fail with HTTP 200, which the status code alone reads
// as a successful empty answer.
func TestResponsesReportsAFailureThatArrivedAsHTTP200(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"failed", `{"status":"failed","error":{"message":"model overloaded","type":"server_error"},"output":[]}`, "model overloaded"},
		{"incomplete", `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`, "max_output_tokens"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, _ := serve(t, c.body)
			_, err := p.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
			if err == nil {
				t.Fatalf("a %s response was reported as success", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestResponsesStreamsTextThenToolCallsThenUsage(t *testing.T) {
	events := strings.Join([]string{
		`data: {"type":"response.created","response":{"status":"in_progress"}}`,
		`data: {"type":"response.output_text.delta","delta":"One "}`,
		`data: {"type":"response.output_text.delta","delta":"two."}`,
		`data: {"type":"response.completed","response":{"status":"completed","output":[` +
			`{"type":"message","content":[{"type":"output_text","text":"One two."}]},` +
			`{"type":"function_call","call_id":"call_a","name":"move","arguments":"{}"}],` +
			`"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(events))
	}))
	defer server.Close()

	p := newResponsesProvider(Config{APIKey: "k", Model: "gpt-5.6-luna", BaseURL: server.URL}, "", true)
	ch, err := p.Stream(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}

	var text strings.Builder
	var calls []ToolCall
	var usage *Usage
	for msg := range ch {
		if msg.Err != nil {
			t.Fatalf("stream error: %v", msg.Err)
		}
		text.WriteString(msg.Content)
		calls = append(calls, msg.ToolCalls...)
		if msg.Usage != nil {
			usage = msg.Usage
		}
	}
	if text.String() != "One two." {
		t.Errorf("streamed text = %q", text.String())
	}
	if len(calls) != 1 || calls[0].ID != "call_a" {
		t.Errorf("tool calls = %+v", calls)
	}
	if usage == nil || usage.TotalTokens != 6 {
		t.Errorf("usage = %+v", usage)
	}
}

func TestResponsesStreamSurfacesAnErrorEvent(t *testing.T) {
	events := "data: {\"type\":\"error\",\"error\":{\"message\":\"upstream died\",\"type\":\"server_error\"}}\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(events))
	}))
	defer server.Close()

	p := newResponsesProvider(Config{APIKey: "k", BaseURL: server.URL}, "", true)
	ch, err := p.Stream(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var got error
	for msg := range ch {
		if msg.Err != nil {
			got = msg.Err
		}
	}
	if got == nil || !strings.Contains(got.Error(), "upstream died") {
		t.Fatalf("stream error = %v, want it to carry the backend's message", got)
	}
}

func TestResponsesDropsServiceTierForNonOpenAIBackends(t *testing.T) {
	for _, c := range []struct {
		name    string
		allowed bool
		model   string
		want    bool
	}{
		{"openai", true, "gpt-5.6-luna", true},
		{"compatible backend", false, "gpt-5.6-luna", false},
		{"router to another vendor", true, "gemini/gemini-3.5-flash", false},
	} {
		tr := &responsesTransport{allowServiceTier: c.allowed}
		if got := tr.serviceTier(ServiceTierFlex, c.model) == "flex"; got != c.want {
			t.Errorf("%s: service_tier sent = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestOpenAIResponsesProviderIsSelectable(t *testing.T) {
	c, err := New(Config{Provider: "openai-responses", APIKey: "k", Model: "gpt-5.6-luna"})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := c.provider.(*responsesProvider)
	if !ok {
		t.Fatalf("provider = %T, want *responsesProvider", c.provider)
	}
	if p.url() != "https://api.openai.com/v1/responses" {
		t.Errorf("url = %q", p.url())
	}
	if len(ReasoningChoices("openai-responses", "gpt-5.6-luna")) == 0 {
		t.Error("the provider offers no reasoning efforts, which is the reason it exists")
	}
}

func pretty(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
