package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A backend that fails after the stream has started reports it in the SSE body,
// not in the HTTP status. Those failures used to end the stream silently and
// look like an empty answer.
func TestStreamSurfacesBackendErrors(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "error object mid-stream",
			payload: `data: {"error":{"message":"upstream provider is down","type":"provider_error","code":502}}`,
			want:    "upstream provider is down (provider_error, code 502)",
		},
		{
			name:    "unreadable chunk",
			payload: `data: {"choices":[{"index":0,`,
			want:    "unreadable chunk",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
				w.(http.Flusher).Flush()
				fmt.Fprint(w, c.payload+"\n\n")
			}))
			defer server.Close()

			client, err := New(Config{Provider: "lmstudio", Model: "test", BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			sess := NewSession(client, "")

			ch, err := sess.StreamWithTools(context.Background(), "hello")
			if err != nil {
				t.Fatal(err)
			}
			var got string
			for msg := range ch {
				got += msg.Content
			}

			if sess.Err() == nil {
				t.Fatalf("Err() = nil, want an error mentioning %q", c.want)
			}
			if !strings.Contains(sess.Err().Error(), c.want) {
				t.Fatalf("Err() = %v, want it to mention %q", sess.Err(), c.want)
			}
			// What did arrive before the failure is still the user's to keep.
			if got != "partial" {
				t.Fatalf("content before the failure = %q, want %q", got, "partial")
			}
		})
	}
}

// An HTTP-level rejection carries the backend's own explanation, which is what
// tells a bad model name apart from a bad key.
func TestStreamSurfacesHTTPErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"message":"model 'nope' not found","code":"model_not_found"}}`)
	}))
	defer server.Close()

	client, err := New(Config{Provider: "lmstudio", Model: "nope", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	sess := NewSession(client, "")
	ch, err := sess.StreamWithTools(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if sess.Err() == nil || !strings.Contains(sess.Err().Error(), "model 'nope' not found") {
		t.Fatalf("Err() = %v, want the backend's own message", sess.Err())
	}
}
