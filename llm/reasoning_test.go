package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A thought-summary delta carries no content of its own. StreamWithTools has
// to forward it anyway, or the harness never sees the model think.
func TestStreamWithToolsForwardsReasoning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"choices":[{"index":0,"delta":{"reasoning":"hmm "}}]}`,
			`{"choices":[{"index":0,"delta":{"reasoning_content":"and so"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"42"}}]}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client, err := New(Config{Provider: "ollama", Model: "test", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	sess := NewSession(client, "")
	ch, err := sess.StreamWithTools(context.Background(), "what is it")
	if err != nil {
		t.Fatal(err)
	}
	var reasoning, content string
	for msg := range ch {
		reasoning += msg.Reasoning
		content += msg.Content
	}
	if err := sess.Err(); err != nil {
		t.Fatal(err)
	}
	if reasoning != "hmm and so" {
		t.Errorf("reasoning is %q, want %q", reasoning, "hmm and so")
	}
	if content != "42" {
		t.Errorf("content is %q, want %q", content, "42")
	}
}

// Every level ollama's validator accepts is offered, including the four names
// OpenAI has no equivalent for, and each goes on the wire verbatim.
func TestReasoningChoicesOllama(t *testing.T) {
	got := ReasoningChoices("ollama", "qwen38-iq4-opencode:latest")
	want := []ReasoningEffort{
		ReasoningEffortNone, ReasoningEffortMinimal, ReasoningEffortLow, ReasoningEffortMedium,
		ReasoningEffortHigh, ReasoningEffortXHigh, ReasoningEffortUltra, ReasoningEffortMax,
	}
	if len(got) != len(want) {
		t.Fatalf("choices = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("choices = %v, want %v", got, want)
		}
		// Only an OpenAI reasoning-tier model rewrites None (to "minimal");
		// against an ollama model every name, None included, has to reach
		// the backend verbatim.
		if wire := openaiReasoningEffort(want[i], "qwen38-iq4-opencode:latest"); wire != string(want[i]) {
			t.Errorf("%s goes on the wire as %q", want[i], wire)
		}
	}
}
