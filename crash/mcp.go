package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/memmaker/gollum/llm"
)

// mcpToolDef is one tool an MCP server advertised via tools/list.
type mcpToolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// mcpHandshakeTimeout bounds initialize + tools/list, at startup or on a menu
// toggle: a hung server should not hang crash's own startup or the menu.
// A var, not a const, so a test can shrink it rather than wait out 10s.
var mcpHandshakeTimeout = 10 * time.Second

// mcpClient talks JSON-RPC 2.0 to one MCP server over stdio: newline-delimited
// messages, not LSP's Content-Length framing.
type mcpClient struct {
	name  string
	cmd   *exec.Cmd // nil when a test wires the transport directly
	tools []mcpToolDef

	stdin   io.WriteCloser
	writeMu sync.Mutex

	nextID  int64
	pendMu  sync.Mutex
	pending map[int64]chan rpcResult

	dead     chan struct{}
	deadOnce sync.Once
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcEnvelope decodes a response. A server-initiated notification (no id) is
// left with ID nil and dropped — logging chatter and the like are not part of
// the surface crash uses.
type rpcEnvelope struct {
	ID     *int64          `json:"id,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

// newMCPClient wraps a live stdin/stdout pair and starts the read loop. cmd is
// nil for a fake transport in tests; spawnMCP supplies the real one.
func newMCPClient(name string, stdin io.WriteCloser, stdout io.Reader, cmd *exec.Cmd) *mcpClient {
	c := &mcpClient{
		name:    name,
		cmd:     cmd,
		stdin:   stdin,
		pending: map[int64]chan rpcResult{},
		dead:    make(chan struct{}),
	}
	go c.readLoop(stdout)
	return c
}

// spawnMCP starts cfg's command, wires its stdio, and runs the handshake
// (initialize, notifications/initialized, tools/list). The process is killed
// if any of that fails, so a bad config never leaves an orphan running.
func spawnMCP(cfg MCPConfig) (*mcpClient, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: %s: %w", cfg.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: %s: %w", cfg.Name, err)
	}
	cmd.Stderr = io.Discard // a chatty server must not garble the alt-screen TUI
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp: %s: %w", cfg.Name, err)
	}

	c := newMCPClient(cfg.Name, stdin, stdout, cmd)
	if err := c.handshake(); err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

// handshake runs initialize, the required initialized notification, then
// tools/list, all bounded by mcpHandshakeTimeout.
func (c *mcpClient) handshake() error {
	ctx, cancel := context.WithTimeout(context.Background(), mcpHandshakeTimeout)
	defer cancel()

	if _, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "crash", "version": "1.0"},
	}); err != nil {
		return fmt.Errorf("mcp: %s: initialize: %w", c.name, err)
	}
	if err := c.notify("notifications/initialized", nil); err != nil {
		return fmt.Errorf("mcp: %s: initialized: %w", c.name, err)
	}

	raw, err := c.call(ctx, "tools/list", nil)
	if err != nil {
		return fmt.Errorf("mcp: %s: tools/list: %w", c.name, err)
	}
	var listed struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		return fmt.Errorf("mcp: %s: tools/list: bad response: %w", c.name, err)
	}
	for _, t := range listed.Tools {
		c.tools = append(c.tools, mcpToolDef{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return nil
}

// callTool invokes one tool over the MCP connection and reduces its content
// blocks to a single string, the shape every other tool executor returns.
func (c *mcpClient) callTool(ctx context.Context, name string, argumentsJSON string) (string, error) {
	var args any = json.RawMessage("{}")
	if strings.TrimSpace(argumentsJSON) != "" {
		args = json.RawMessage(argumentsJSON)
	}
	raw, err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", err
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("mcp: %s: tools/call: bad response: %w", c.name, err)
	}
	var parts []string
	for _, block := range res.Content {
		if block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	text := strings.Join(parts, "\n")
	if res.IsError {
		return "", fmt.Errorf("%s", text)
	}
	return text, nil
}

// call sends a request and blocks for its response, the process dying, or ctx
// expiring, whichever comes first.
func (c *mcpClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	ch := make(chan rpcResult, 1)
	c.pendMu.Lock()
	c.pending[id] = ch
	c.pendMu.Unlock()

	if err := c.write(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
		return nil, err
	}

	select {
	case res := <-ch:
		return res.result, res.err
	case <-c.dead:
		return nil, fmt.Errorf("mcp: %s exited", c.name)
	case <-ctx.Done():
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
		return nil, ctx.Err()
	}
}

// notify sends a request with no id, which gets no response.
func (c *mcpClient) notify(method string, params any) error {
	return c.write(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *mcpClient) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

// readLoop dispatches responses to whichever call is waiting on their id,
// until the pipe closes — the server exited or was killed.
func (c *mcpClient) readLoop(r io.Reader) {
	defer c.markDead()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var env rpcEnvelope
		if err := json.Unmarshal(line, &env); err != nil || env.ID == nil {
			continue
		}
		c.pendMu.Lock()
		ch, ok := c.pending[*env.ID]
		if ok {
			delete(c.pending, *env.ID)
		}
		c.pendMu.Unlock()
		if !ok {
			continue
		}
		if env.Error != nil {
			ch <- rpcResult{err: env.Error}
		} else {
			ch <- rpcResult{result: env.Result}
		}
	}
}

// markDead unblocks every pending call once, the first time the connection is
// known to be gone.
func (c *mcpClient) markDead() {
	c.deadOnce.Do(func() {
		close(c.dead)
		c.pendMu.Lock()
		for id, ch := range c.pending {
			ch <- rpcResult{err: fmt.Errorf("mcp: %s exited", c.name)}
			delete(c.pending, id)
		}
		c.pendMu.Unlock()
	})
}

// close kills the subprocess, if any, and stops writes. Safe to call more
// than once.
func (c *mcpClient) close() {
	if c.cmd != nil && c.cmd.Process != nil {
		c.cmd.Process.Kill()
		go c.cmd.Wait() // reap; nothing to report
	}
	c.stdin.Close()
}

// isDead reports whether the read loop has already ended — the process
// exited or its pipe broke. A failed tool call only means the MCP itself is
// gone when this is true; an ordinary tool-side error (bad arguments, a
// file not found) must not disable anything.
func (c *mcpClient) isDead() bool {
	select {
	case <-c.dead:
		return true
	default:
		return false
	}
}

// mcpDeadMsg reports that a tool call discovered its MCP's connection is
// gone, so the menu and session can be brought back in step with reality.
// Arrives on the event loop because the discovery happens on whichever
// goroutine ran the tool call, not on the TUI's own.
type mcpDeadMsg struct {
	name string
	err  error
}

func notifyMCPDead(name string, err error) {
	if prog == nil {
		return
	}
	prog.Send(mcpDeadMsg{name: name, err: err})
}

// registerMCPTools adds every tool an MCP server advertised to the session,
// skipping any the user has individually disabled from the Tools menu, each
// one routed back through this client's stdio connection. A call that fails
// because the connection itself is gone (not an ordinary tool error) reports
// it so the TUI can mark the MCP disabled and tell the user.
func registerMCPTools(sess *llm.Session, c *mcpClient, toolConfigs []ToolConfig) {
	for _, t := range c.tools {
		t := t
		if !toolEnabled(t.Name, toolConfigs) {
			continue
		}
		sess.AddTool(llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		}, func(ctx context.Context, call llm.ToolCall) (string, error) {
			result, err := c.callTool(ctx, t.Name, call.Function.Arguments)
			if err != nil && c.isDead() {
				notifyMCPDead(c.name, err)
			}
			return result, err
		})
	}
}

// startMCPs spawns every enabled MCP config and registers its tools on sess.
// A server that fails to start has its config flipped to disabled — so the
// MCPs menu reflects reality — and its error appended to the returned notes;
// startup itself is never blocked by one bad server.
func startMCPs(sess *llm.Session, cfgs []MCPConfig, toolConfigs []ToolConfig) (map[string]*mcpClient, []string) {
	clients := map[string]*mcpClient{}
	var notes []string
	for i := range cfgs {
		if !cfgs[i].Enabled {
			continue
		}
		c, err := spawnMCP(cfgs[i])
		if err != nil {
			cfgs[i].Enabled = false
			notes = append(notes, err.Error())
			continue
		}
		clients[cfgs[i].Name] = c
		registerMCPTools(sess, c, toolConfigs)
	}
	return clients, notes
}
