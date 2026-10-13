package llm

import (
	"context"
	"testing"
	"time"
)

// scriptedProvider replays one canned stream per request, so a tool round trip
// can be inspected without a backend.
type scriptedProvider struct {
	streams  [][]Message
	seen     [][]Message // the messages sent with each request
	seenTool [][]Tool    // the tools sent with each request
}

func (p *scriptedProvider) Chat(context.Context, Request) (Response, error) {
	return Response{}, nil
}

func (p *scriptedProvider) SetTimeout(time.Duration) {}

func (p *scriptedProvider) Stream(_ context.Context, req Request) (<-chan Message, error) {
	p.seen = append(p.seen, req.Messages)
	p.seenTool = append(p.seenTool, req.Tools)
	ch := make(chan Message, 4)
	if len(p.streams) > 0 {
		for _, m := range p.streams[0] {
			ch <- m
		}
		p.streams = p.streams[1:]
	}
	close(ch)
	return ch, nil
}

// A tool result answers a tool call, and the backend has to see the call that
// it answers. Sending the result on its own made providers ask for the very
// same call again on the follow-up request.
func TestStreamWithToolsSendsCallBeforeResult(t *testing.T) {
	call := ToolCall{ID: "call_1", Function: ToolCallFunction{Name: "pick", Arguments: "{}"}}
	p := &scriptedProvider{streams: [][]Message{
		{{Role: RoleAssistant, ToolCalls: []ToolCall{call}}},
		{{Role: RoleAssistant, Content: "a cat then"}},
	}}
	s := NewSession(&Client{provider: p}, "")
	s.AddTool(Tool{Type: "function", Function: ToolFunction{Name: "pick"}},
		func(context.Context, ToolCall) (string, error) { return "cat", nil })

	ch, err := s.StreamWithTools(context.Background(), "choose an animal")
	if err != nil {
		t.Fatal(err)
	}
	var streamed []Message
	for msg := range ch {
		streamed = append(streamed, msg)
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}

	if len(p.seen) != 2 {
		t.Fatalf("provider saw %d requests, want 2", len(p.seen))
	}
	follow := p.seen[1]
	var callIdx, resultIdx = -1, -1
	for i, m := range follow {
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			callIdx = i
		}
		if m.Role == RoleTool {
			resultIdx = i
		}
	}
	if callIdx < 0 {
		t.Fatal("follow-up request carried no assistant tool call")
	}
	if resultIdx < 0 {
		t.Fatal("follow-up request carried no tool result")
	}
	if callIdx > resultIdx {
		t.Fatalf("tool result at %d precedes its call at %d", resultIdx, callIdx)
	}
	if got := follow[resultIdx].Content; got != "cat" {
		t.Fatalf("tool result was %q, want %q", got, "cat")
	}

	// The harness needs to see the activity too, to be able to draw it.
	var sawCall, sawResult bool
	for _, m := range streamed {
		if len(m.ToolCalls) > 0 {
			sawCall = true
		}
		if m.Role == RoleTool && m.Content == "cat" {
			sawResult = true
		}
	}
	if !sawCall || !sawResult {
		t.Fatalf("stream hid the tool round trip (call=%v result=%v)", sawCall, sawResult)
	}
}

// A tool-calling response's trailing usage chunk arrives after the
// ToolCalls message, on the same stream. Cancelling the request the instant
// ToolCalls is seen used to race that chunk and drop it, undercounting every
// tool-using turn's cost.
func TestStreamWithToolsKeepsUsageFromToolCallTurn(t *testing.T) {
	call := ToolCall{ID: "call_1", Function: ToolCallFunction{Name: "pick", Arguments: "{}"}}
	p := &scriptedProvider{streams: [][]Message{
		{
			{Role: RoleAssistant, ToolCalls: []ToolCall{call}},
			{Role: RoleAssistant, Usage: &Usage{TotalTokens: 42}},
		},
		{{Role: RoleAssistant, Content: "a cat then"}},
	}}
	s := NewSession(&Client{provider: p}, "")
	s.AddTool(Tool{Type: "function", Function: ToolFunction{Name: "pick"}},
		func(context.Context, ToolCall) (string, error) { return "cat", nil })

	ch, err := s.StreamWithTools(context.Background(), "choose an animal")
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	if got := s.Usage().TotalTokens; got != 42 {
		t.Fatalf("usage = %d, want 42 (tool-call turn's usage chunk was dropped)", got)
	}
}

// Tools are offered by default, and turning them off has to reach the wire —
// a model that is still shown a tool will still call it.
func TestSetToolsEnabled(t *testing.T) {
	newSession := func(p *scriptedProvider) *Session {
		s := NewSession(&Client{provider: p}, "")
		s.AddTool(Tool{Type: "function", Function: ToolFunction{Name: "pick"}},
			func(context.Context, ToolCall) (string, error) { return "cat", nil })
		return s
	}
	drain := func(s *Session, p *scriptedProvider) {
		t.Helper()
		ch, err := s.StreamWithTools(context.Background(), "hello")
		if err != nil {
			t.Fatal(err)
		}
		for range ch {
		}
	}

	p := &scriptedProvider{streams: [][]Message{{{Role: RoleAssistant, Content: "hi"}}}}
	s := newSession(p)
	if !s.SendRegisteredTools {
		t.Fatal("tools should be on by default")
	}
	drain(s, p)
	if len(p.seenTool[0]) != 1 {
		t.Fatalf("sent %d tools by default, want 1", len(p.seenTool[0]))
	}

	p = &scriptedProvider{streams: [][]Message{{{Role: RoleAssistant, Content: "hi"}}}}
	s = newSession(p)
	s.SendRegisteredTools = false
	drain(s, p)
	if len(p.seenTool[0]) != 0 {
		t.Fatalf("sent %d tools with tools off, want none", len(p.seenTool[0]))
	}

	// And the registrations survive, so it can be turned back on.
	s.SendRegisteredTools = true
	p.streams = [][]Message{{{Role: RoleAssistant, Content: "hi"}}}
	drain(s, p)
	if len(p.seenTool[1]) != 1 {
		t.Fatalf("re-enabling sent %d tools, want 1", len(p.seenTool[1]))
	}
}

// A name collision must not leave two entries with the same name on the
// wire — that is not "last one wins," it is a duplicate listing a provider
// may reject outright.
func TestAddToolReplacesSameName(t *testing.T) {
	p := &scriptedProvider{streams: [][]Message{{{Role: RoleAssistant, Content: "hi"}}}}
	s := NewSession(&Client{provider: p}, "")
	s.AddTool(Tool{Type: "function", Function: ToolFunction{Name: "read", Description: "first"}},
		func(context.Context, ToolCall) (string, error) { return "first", nil })
	s.AddTool(Tool{Type: "function", Function: ToolFunction{Name: "read", Description: "second"}},
		func(context.Context, ToolCall) (string, error) { return "second", nil })

	if len(s.tools) != 1 {
		t.Fatalf("got %d tools named \"read\", want 1: %+v", len(s.tools), s.tools)
	}
	if s.tools[0].Function.Description != "second" {
		t.Fatalf("surviving definition is %q, want the second registration to win", s.tools[0].Function.Description)
	}

	ch, err := s.StreamWithTools(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if len(p.seenTool[0]) != 1 {
		t.Fatalf("sent %d tools named \"read\" on the wire, want 1", len(p.seenTool[0]))
	}

	result, err := s.executors["read"](context.Background(), ToolCall{})
	if err != nil || result != "second" {
		t.Fatalf("executor = (%q, %v), want the second registration's executor to win", result, err)
	}
}

// RemoveTool is how an MCP toggled off in the menu stops being offered — it
// must drop both the wire listing and the executor a stray call would hit.
func TestRemoveTool(t *testing.T) {
	p := &scriptedProvider{streams: [][]Message{{{Role: RoleAssistant, Content: "hi"}}}}
	s := NewSession(&Client{provider: p}, "")
	s.AddTool(Tool{Type: "function", Function: ToolFunction{Name: "pick"}},
		func(context.Context, ToolCall) (string, error) { return "cat", nil })
	s.AddTool(Tool{Type: "function", Function: ToolFunction{Name: "keep"}},
		func(context.Context, ToolCall) (string, error) { return "kept", nil })

	s.RemoveTool("pick")
	// Removing a name that was never registered must not panic or touch
	// anything else.
	s.RemoveTool("never-registered")

	ch, err := s.StreamWithTools(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if len(p.seenTool[0]) != 1 || p.seenTool[0][0].Function.Name != "keep" {
		t.Fatalf("offered tools after remove = %+v, want only \"keep\"", p.seenTool[0])
	}
	if _, ok := s.executors["pick"]; ok {
		t.Fatal("removed tool's executor is still registered")
	}
}
