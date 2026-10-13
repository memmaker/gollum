package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// systemText decodes the systemInstruction parts of a built request and
// concatenates their text, mirroring what Gemini receives on the wire.
func systemText(t *testing.T, g geminiReq) string {
	t.Helper()
	if g.SystemInstruction == nil {
		return ""
	}
	var parts []geminiPart
	if err := json.Unmarshal(g.SystemInstruction.Parts, &parts); err != nil {
		t.Fatalf("unmarshal systemInstruction parts: %v", err)
	}
	var out string
	for _, p := range parts {
		out += p.Text
	}
	return out
}

func TestBuildGeminiReqSystemInstruction(t *testing.T) {
	tests := []struct {
		name       string
		messages   []Message
		wantSystem string
		wantNil    bool
	}{
		{
			name: "single system message is passed through unchanged",
			messages: []Message{
				{Role: RoleSystem, Content: "you are a helpful assistant"},
				{Role: RoleUser, Content: "hi"},
			},
			wantSystem: "you are a helpful assistant",
		},
		{
			name: "multiple system messages are joined in order",
			messages: []Message{
				{Role: RoleSystem, Content: "first prompt"},
				{Role: RoleSystem, Content: "second prompt"},
				{Role: RoleUser, Content: "hi"},
			},
			wantSystem: "first prompt\n\nsecond prompt",
		},
		{
			name: "system messages interleaved with other roles keep their order",
			messages: []Message{
				{Role: RoleSystem, Content: "first prompt"},
				{Role: RoleUser, Content: "hi"},
				{Role: RoleAssistant, Content: "hello"},
				{Role: RoleSystem, Content: "second prompt"},
				{Role: RoleUser, Content: "again"},
			},
			wantSystem: "first prompt\n\nsecond prompt",
		},
		{
			name: "no system message leaves systemInstruction unset",
			messages: []Message{
				{Role: RoleUser, Content: "hi"},
			},
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := buildGeminiReq(Request{Messages: tt.messages})

			if tt.wantNil {
				if g.SystemInstruction != nil {
					t.Fatalf("SystemInstruction = %q, want nil", systemText(t, g))
				}
				return
			}

			if g.SystemInstruction == nil {
				t.Fatal("SystemInstruction = nil, want non-nil")
			}
			if got := systemText(t, g); got != tt.wantSystem {
				t.Errorf("systemInstruction = %q, want %q", got, tt.wantSystem)
			}
		})
	}
}

func TestBuildGeminiReqSystemMessagesStayOutOfContents(t *testing.T) {
	g := buildGeminiReq(Request{Messages: []Message{
		{Role: RoleSystem, Content: "first prompt"},
		{Role: RoleSystem, Content: "second prompt"},
		{Role: RoleUser, Content: "hi"},
	}})

	if len(g.Contents) != 1 {
		t.Fatalf("len(Contents) = %d, want 1", len(g.Contents))
	}
	if g.Contents[0].Role != "user" {
		t.Errorf("Contents[0].Role = %q, want %q", g.Contents[0].Role, "user")
	}
}

// Gemini repeats usageMetadata on every chunk with running totals: the stream
// must report the last one once, not add them up.
func TestGeminiStreamUsageOnce(t *testing.T) {
	body := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":1,\"totalTokenCount\":11}}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" there\"}]}}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":4,\"totalTokenCount\":14,\"thoughtsTokenCount\":3}}\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
	}))
	defer srv.Close()

	p := &geminiProvider{baseURL: srv.URL, apiKey: "k", client: srv.Client()}
	ch, err := p.Stream(context.Background(), Request{Model: "gemini-x", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var got []Usage
	for msg := range ch {
		if msg.Usage != nil {
			got = append(got, *msg.Usage)
		}
	}
	if len(got) != 1 {
		t.Fatalf("usage messages = %d, want 1: %+v", len(got), got)
	}
	if want := (Usage{InputTokens: 10, OutputTokens: 4, TotalTokens: 14, ReasoningTokens: 3}); got[0] != want {
		t.Fatalf("usage = %+v, want %+v", got[0], want)
	}
}

func TestBuildGeminiReqToolChoice(t *testing.T) {
	tools := []Tool{{Type: "function", Function: ToolFunction{Name: "roll"}}}

	g := buildGeminiReq(Request{Tools: tools, ToolChoice: ToolChoiceRequired})
	if g.ToolConfig == nil || g.ToolConfig.FunctionCallingConfig.Mode != "ANY" {
		t.Fatalf("required tool choice should map to mode ANY, got %+v", g.ToolConfig)
	}

	if g := buildGeminiReq(Request{Tools: tools}); g.ToolConfig != nil {
		t.Fatalf("unset tool choice should send no toolConfig, got %+v", g.ToolConfig)
	}
	if g := buildGeminiReq(Request{ToolChoice: ToolChoiceRequired}); g.ToolConfig != nil {
		t.Fatalf("no tools means no toolConfig, got %+v", g.ToolConfig)
	}
}
