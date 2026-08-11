package llm

import (
	"context"
	"testing"
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
