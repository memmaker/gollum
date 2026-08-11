package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// statelessServer answers every request with one line of content and records
// the messages it was sent.
func statelessServer(t *testing.T, seen *[][]Message) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		*seen = append(*seen, req.Messages)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
		w.(http.Flusher).Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// Stateless drops the conversation from the wire but not from the session: the
// point of the switch is that turning it back off resumes with everything that
// was said while it was on.
func TestStatelessKeepsTranscript(t *testing.T) {
	var seen [][]Message
	server := statelessServer(t, &seen)
	defer server.Close()

	client, err := New(Config{Provider: "lmstudio", Model: "test", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	sess := NewSession(client, "be brief")

	ask := func(prompt string) {
		t.Helper()
		ch, err := sess.StreamWithTools(context.Background(), prompt)
		if err != nil {
			t.Fatal(err)
		}
		for range ch {
		}
		if err := sess.Err(); err != nil {
			t.Fatal(err)
		}
	}

	sess.Stateless = true
	ask("first")
	ask("second")

	// Each request carried the system prompt and its own question, nothing else.
	for i, msgs := range seen {
		if len(msgs) != 2 {
			t.Fatalf("request %d sent %d messages, want the system prompt and the question:\n%+v", i, len(msgs), msgs)
		}
		if msgs[0].Role != RoleSystem || msgs[0].Content != "be brief" {
			t.Fatalf("request %d lost the system prompt: %+v", i, msgs[0])
		}
	}
	if got := seen[1][1].Content; got != "second" {
		t.Fatalf("second request asked %q", got)
	}

	// The transcript grew all the same: system, two questions, two answers.
	hist := sess.History()
	if len(hist) != 5 {
		t.Fatalf("transcript holds %d messages, want 5:\n%+v", len(hist), hist)
	}
	want := []string{"be brief", "first", "ok", "second", "ok"}
	for i, w := range want {
		if hist[i].Content != w {
			t.Fatalf("transcript[%d] = %q, want %q\n%+v", i, hist[i].Content, w, hist)
		}
	}

	// Switching back sends the lot.
	sess.Stateless = false
	ask("third")
	if got := len(seen[2]); got != 6 {
		t.Fatalf("resumed request sent %d messages, want the 5 recorded plus the new question:\n%+v", got, seen[2])
	}
}
