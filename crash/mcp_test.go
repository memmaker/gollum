package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// fakeServer replays canned JSON-RPC responses over an in-process pipe,
// keyed by method, so the client's handshake and call/notify plumbing can be
// tested without a real subprocess.
type fakeServer struct {
	toClient   *io.PipeWriter
	fromClient *bufio.Scanner
	handlers   map[string]func(id json.RawMessage, params json.RawMessage) any
}

func newFakeServer(t *testing.T) (*fakeServer, *mcpClient) {
	t.Helper()
	serverReadsFromClient, clientToServer := io.Pipe()
	clientReadsFromServer, serverToClient := io.Pipe()

	fs := &fakeServer{
		toClient:   serverToClient,
		fromClient: bufio.NewScanner(serverReadsFromClient),
		handlers:   map[string]func(json.RawMessage, json.RawMessage) any{},
	}
	c := newMCPClient("fake", clientToServer, clientReadsFromServer, nil)
	t.Cleanup(func() { c.close() })
	return fs, c
}

// serve runs the request/notification loop until the client pipe closes.
// Call as a goroutine before issuing any client call that expects a reply.
func (fs *fakeServer) serve() {
	for fs.fromClient.Scan() {
		var env struct {
			ID     json.RawMessage `json:"id,omitempty"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params,omitempty"`
		}
		if err := json.Unmarshal(fs.fromClient.Bytes(), &env); err != nil {
			continue
		}
		if len(env.ID) == 0 {
			continue // a notification: nothing to answer
		}
		h, ok := fs.handlers[env.Method]
		if !ok {
			continue
		}
		resp, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(env.ID),
			"result":  h(env.ID, env.Params),
		})
		fs.toClient.Write(append(resp, '\n'))
	}
}

func stdHandshakeHandlers(tools any) map[string]func(json.RawMessage, json.RawMessage) any {
	return map[string]func(json.RawMessage, json.RawMessage) any{
		"initialize": func(json.RawMessage, json.RawMessage) any {
			return map[string]any{"protocolVersion": "2024-11-05"}
		},
		"tools/list": func(json.RawMessage, json.RawMessage) any {
			return map[string]any{"tools": tools}
		},
	}
}

func TestHandshakeListsTools(t *testing.T) {
	fs, c := newFakeServer(t)
	fs.handlers = stdHandshakeHandlers([]map[string]any{
		{"name": "read", "description": "read a file", "inputSchema": map[string]any{"type": "object"}},
	})
	go fs.serve()

	if err := c.handshake(); err != nil {
		t.Fatal(err)
	}
	if len(c.tools) != 1 || c.tools[0].Name != "read" || c.tools[0].Description != "read a file" {
		t.Fatalf("tools after handshake = %+v", c.tools)
	}
}

func TestCallToolJoinsTextBlocks(t *testing.T) {
	fs, c := newFakeServer(t)
	fs.handlers = stdHandshakeHandlers([]map[string]any{{"name": "read"}})
	fs.handlers["tools/call"] = func(_ json.RawMessage, params json.RawMessage) any {
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "line one"},
				{"type": "text", "text": "line two"},
			},
			"isError": false,
		}
	}
	go fs.serve()

	if err := c.handshake(); err != nil {
		t.Fatal(err)
	}
	got, err := c.callTool(context.Background(), "read", `{"file":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "line one\nline two"; got != want {
		t.Fatalf("callTool result = %q, want %q", got, want)
	}
}

func TestCallToolIsErrorBecomesGoError(t *testing.T) {
	fs, c := newFakeServer(t)
	fs.handlers = stdHandshakeHandlers([]map[string]any{{"name": "read"}})
	fs.handlers["tools/call"] = func(_ json.RawMessage, _ json.RawMessage) any {
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": "file not found"}},
			"isError": true,
		}
	}
	go fs.serve()

	if err := c.handshake(); err != nil {
		t.Fatal(err)
	}
	_, err := c.callTool(context.Background(), "read", `{}`)
	if err == nil || err.Error() != "file not found" {
		t.Fatalf("callTool err = %v, want \"file not found\"", err)
	}
}

// A server that never answers must not hang the caller forever — the menu
// toggle and startup both depend on the handshake giving up on its own.
func TestHandshakeTimesOutOnSilentServer(t *testing.T) {
	fs, c := newFakeServer(t)
	// No handlers registered: serve() reads and drops every request rather
	// than answering, so writes do not block on the unbuffered pipe but no
	// response ever arrives either.
	go fs.serve()

	orig := mcpHandshakeTimeout
	mcpHandshakeTimeout = 100 * time.Millisecond
	defer func() { mcpHandshakeTimeout = orig }()

	if err := c.handshake(); err == nil {
		t.Fatal("handshake against a silent server should time out")
	}
}

// A dead pipe must unblock any call in flight rather than leaving it stuck.
func TestCallReturnsErrorAfterProcessExit(t *testing.T) {
	fs, c := newFakeServer(t)
	fs.handlers = stdHandshakeHandlers([]map[string]any{})
	go fs.serve()

	if err := c.handshake(); err != nil {
		t.Fatal(err)
	}
	fs.toClient.Close() // the server side hangs up, as if the process died

	_, err := c.callTool(context.Background(), "read", `{}`)
	if err == nil {
		t.Fatal("callTool after the server exited should return an error, not hang")
	}
}

// TestSpawnRealHashline runs the whole path against the actual `hashline mcp`
// binary this feature was built for, skipping if it is not on PATH — CI has
// no reason to have it installed, but a dev machine that does gets a real
// end-to-end check instead of only the faked-transport tests above.
func TestSpawnRealHashline(t *testing.T) {
	if _, err := exec.LookPath("hashline"); err != nil {
		t.Skip("hashline not on PATH")
	}

	c, err := spawnMCP(MCPConfig{Name: "hashline", Command: "hashline", Args: []string{"mcp"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()

	var names []string
	for _, tool := range c.tools {
		names = append(names, tool.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "read") {
		t.Fatalf("hashline tools = %v, want a \"read\" tool among them", names)
	}
}
