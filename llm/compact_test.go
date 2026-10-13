package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// compactServer answers the summarizing call with a fixed summary and every
// other call with one line of streamed content, recording what it was sent
// either way. A request is the summarizing one exactly when its system
// message is CompactionPrompt — which is itself part of what the tests here
// are checking.
func compactServer(t *testing.T, seen *[][]Message) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		*seen = append(*seen, req.Messages)

		if len(req.Messages) > 0 && req.Messages[0].Content == CompactionPrompt {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"THE SUMMARY"}}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
		w.(http.Flusher).Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func compactSession(t *testing.T, seen *[][]Message) (*Session, func()) {
	t.Helper()
	server := compactServer(t, seen)
	client, err := New(Config{Provider: "lmstudio", Model: "test", BaseURL: server.URL})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	sess := NewSession(client, "be brief")
	return sess, server.Close
}

func ask(t *testing.T, sess *Session, prompt string) []Message {
	t.Helper()
	ch, err := sess.StreamWithTools(context.Background(), prompt)
	if err != nil {
		t.Fatal(err)
	}
	var got []Message
	for msg := range ch {
		got = append(got, msg)
	}
	if err := sess.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// A request over the threshold is summarized before it is sent, and what
// reaches the backend afterwards is the summary alone — the whole point being
// that the history it replaced does not go on the wire again.
func TestAutoCompactReplacesHistory(t *testing.T) {
	var seen [][]Message
	sess, done := compactSession(t, &seen)
	defer done()

	ask(t, sess, "first prompt")

	sess.AutoCompact = true
	sess.ContextLimit = 1 // anything at all is over the threshold

	var compacted *Message
	for _, msg := range ask(t, sess, "second prompt") {
		if msg.Compacted {
			m := msg
			compacted = &m
		}
	}
	if compacted == nil {
		t.Fatal("no compaction notice reached the caller")
	}
	if compacted.Err != nil {
		t.Fatalf("compaction failed: %v", compacted.Err)
	}
	if compacted.Content != "THE SUMMARY" {
		t.Errorf("notice carried %q, want the summary", compacted.Content)
	}

	// Three requests: the first turn, the summarizing call, the second turn.
	if len(seen) != 3 {
		t.Fatalf("got %d requests, want 3", len(seen))
	}
	summarizing, after := seen[1], seen[2]

	if summarizing[0].Role != RoleSystem || summarizing[0].Content != CompactionPrompt {
		t.Errorf("summarizing call's system message is %q, want the compaction prompt", summarizing[0].Content)
	}
	for _, m := range summarizing[1:] {
		if m.Role == RoleSystem {
			t.Error("the original system prompt was still sent with the summarizing call")
		}
	}
	if !strings.Contains(summarizing[1].Content, "first prompt") {
		t.Error("the conversation being summarized was not sent with the summarizing call")
	}

	if len(after) != 2 {
		t.Fatalf("the request after compaction carried %d messages, want the system prompt and the summary", len(after))
	}
	if after[0].Role != RoleSystem || after[0].Content != "be brief" {
		t.Errorf("the original system prompt did not survive compaction: %+v", after[0])
	}
	if !strings.Contains(after[1].Content, "THE SUMMARY") || !strings.Contains(after[1].Content, compactedOutro) {
		t.Errorf("the summary message is %q, want the summary plus the instruction to continue", after[1].Content)
	}
	for _, m := range after {
		if strings.Contains(m.Content, "first prompt") {
			t.Error("the compacted history was sent again")
		}
	}

	// The transcript is the summary too, or the next turn rebuilds exactly
	// the history this just dropped. What the turn produced after the
	// compaction is on the end of it, as on any other turn.
	hist := sess.History()
	if len(hist) != 3 || !strings.Contains(hist[1].Content, "THE SUMMARY") || hist[2].Content != "ok" {
		t.Errorf("history after compaction is %+v, want the system prompt, the summary, and this turn's answer", hist)
	}
	for _, m := range hist {
		if strings.Contains(m.Content, "first prompt") {
			t.Error("the compacted history is still in the transcript")
		}
	}
}

// Compaction is off unless it is switched on and the window is known: a
// guessed limit is not a reason to throw a conversation away.
func TestNeedsCompactionGuards(t *testing.T) {
	sess := &Session{}
	msgs := []Message{
		{Role: RoleSystem, Content: strings.Repeat("x", 4000)},
		{Role: RoleUser, Content: strings.Repeat("x", 4000)},
		{Role: RoleAssistant, Content: strings.Repeat("x", 4000)},
		{Role: RoleUser, Content: strings.Repeat("x", 4000)},
	}
	for _, tc := range []struct {
		name  string
		setup func()
		want  bool
	}{
		{"off by default", func() {}, false},
		{"no limit known", func() { sess.AutoCompact = true }, false},
		{"under the threshold", func() { sess.ContextLimit = 1_000_000 }, false},
		{"over the threshold", func() { sess.ContextLimit = 1000 }, true},
		{"stateless is left alone", func() { sess.Stateless = true }, false},
	} {
		tc.setup()
		if got := sess.needsCompaction(msgs, nil); got != tc.want {
			t.Errorf("%s: needsCompaction = %v, want %v", tc.name, got, tc.want)
		}
	}

	// A conversation already down to its summary cannot be summarized
	// smaller, however far over the threshold it is.
	sess.Stateless = false
	if sess.needsCompaction(msgs[:2], nil) {
		t.Error("compacted a conversation that was already just its summary")
	}
}

// The estimate is scaled by what the backend's own tokenizer reported, so it
// tracks a model that counts differently than four characters to the token.
func TestCalibrateScalesEstimate(t *testing.T) {
	sess := &Session{}
	msgs := []Message{{Role: RoleUser, Content: strings.Repeat("x", 4000)}}

	raw := sess.estimateTokens(msgs, nil)
	if raw < 900 || raw > 1100 {
		t.Fatalf("uncalibrated estimate is %d, want roughly 1000", raw)
	}

	sess.calibrate(Usage{InputTokens: 2 * raw}, msgs, nil)
	if got := sess.estimateTokens(msgs, nil); got < 2*raw-2 || got > 2*raw+2 {
		t.Errorf("calibrated estimate is %d, want roughly %d", got, 2*raw)
	}

	// A report with no input count says nothing and must not reset the ratio.
	sess.calibrate(Usage{OutputTokens: 5}, msgs, nil)
	if got := sess.estimateTokens(msgs, nil); got < 2*raw-2 {
		t.Errorf("an empty usage report dropped the calibration: %d", got)
	}
}

// Tool schemas ride along with every request and are not small; leaving them
// out of the estimate is how a session with a few MCP servers sails past its
// window while the count says it has room.
func TestEstimateCountsTools(t *testing.T) {
	sess := &Session{}
	msgs := []Message{{Role: RoleUser, Content: "hi"}}
	tools := []Tool{{Type: "function", Function: ToolFunction{
		Name:        "bash",
		Description: strings.Repeat("d", 2000),
	}}}
	if with, without := sess.estimateTokens(msgs, tools), sess.estimateTokens(msgs, nil); with <= without+400 {
		t.Errorf("estimate with tools %d, without %d — the schema was not counted", with, without)
	}
}
