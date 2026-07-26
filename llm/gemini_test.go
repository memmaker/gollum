package llm

import (
	"encoding/json"
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
