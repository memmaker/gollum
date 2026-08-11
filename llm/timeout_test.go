package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestStreamIdleTimeout covers the case that motivated the timeout: a backend
// that accepts the request, sends a chunk, and then goes quiet forever.
func TestStreamIdleTimeout(t *testing.T) {
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-done // never finishes the stream
	}))
	// Release the stuck handler before closing, or Close waits on it forever.
	defer func() { close(done); server.Close() }()

	client, err := New(Config{Provider: "lmstudio", Model: "test", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	sess := NewSession(client, "")
	sess.Timeout = 150 * time.Millisecond

	ch, err := sess.StreamWithTools(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}

	var got string
	deadline := time.After(5 * time.Second)
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				if got != "hi" {
					t.Fatalf("content before the stall = %q, want %q", got, "hi")
				}
				if !errors.Is(sess.Err(), ErrTimeout) {
					t.Fatalf("Err() = %v, want ErrTimeout", sess.Err())
				}
				return
			}
			got += msg.Content
		case <-deadline:
			t.Fatal("the stall was never abandoned: control never came back")
		}
	}
}

// TestStreamKeepsGoingWhileDataFlows: chunks arriving slower than the timeout
// are fine, because each one restarts the clock.
func TestStreamKeepsGoingWhileDataFlows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"%d\"}}]}\n\n", i)
			w.(http.Flusher).Flush()
			time.Sleep(60 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client, err := New(Config{Provider: "lmstudio", Model: "test", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	sess := NewSession(client, "")
	sess.Timeout = 150 * time.Millisecond // shorter than the total, longer than a gap

	ch, err := sess.StreamWithTools(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for msg := range ch {
		got += msg.Content
	}
	if got != "01234" {
		t.Fatalf("content = %q, want %q", got, "01234")
	}
	if sess.Err() != nil {
		t.Fatalf("Err() = %v, want nil", sess.Err())
	}
}
