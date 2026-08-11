package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// ErrTimeout reports that a backend went silent past the session's Timeout.
// Callers test for it with errors.Is to tell a dead provider apart from a
// refused request.
var ErrTimeout = errors.New("llm: backend stopped responding")

// DefaultTimeout bounds how long a request may go without producing anything.
// Free, auto-routed backends sometimes accept a request and then never send a
// token; without this the session waits forever and the prompt never returns.
const DefaultTimeout = 120 * time.Second

type ToolExecutor func(ctx context.Context, call ToolCall) (string, error)

type Session struct {
	client          *Client
	messages        []Message
	Model           string
	Temp            float64
	MaxTokens       int
	ReasoningEffort ReasoningEffort
	ServiceTier     ServiceTier
	// Timeout is the idle limit: a stream is abandoned when nothing arrives for
	// this long. It is not a limit on the whole answer, so a model that keeps
	// producing output is left alone however long it takes. Zero disables it.
	Timeout   time.Duration
	tools     []Tool
	executors map[string]ToolExecutor

	// SendRegisteredTools controls whether the tools added with AddTool are
	// offered to the model. NewSession turns it on. Setting it false suppresses
	// them without forgetting the registrations, so tool use can be switched
	// off and back on mid-conversation.
	SendRegisteredTools bool

	// Stateless leaves the conversation out of the request: each turn is sent
	// with the system prompt and the new user message alone. The transcript is
	// still recorded either way, so clearing this resumes the conversation with
	// everything that was said while it was set.
	Stateless bool

	// mu guards messages and the three fields below. StreamWithTools writes them
	// from its own goroutine while the caller reads them through History, Err,
	// LastRequest and Usage, and not every write happens-before the channel
	// close the caller waits on.
	mu sync.Mutex

	// lastErr holds the error that ended the most recent StreamWithTools call.
	// Streaming hands the caller a channel, so a failure mid-flight has nowhere
	// else to go — without this a 502 from the backend looked like an empty
	// answer.
	lastErr error

	// lastRequest is the exact message list handed to the backend on the most
	// recent call, tool results and all. Kept so a caller can show what the
	// model actually saw, which the conversation history alone does not say.
	lastRequest []Message

	// usage accumulates every token count the backends reported for this
	// session. Providers that report none leave it at zero.
	usage Usage
}

// Usage returns the tokens this session has spent so far, summed over every
// call a backend reported counts for.
func (s *Session) Usage() Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage
}

// LastRequest returns the messages sent with the most recent request. The
// slice is a snapshot: the session replaces it wholesale on the next request
// rather than appending to it.
func (s *Session) LastRequest() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastRequest
}

// Err returns the error that ended the last StreamWithTools call, or nil. Safe
// to call while a stream is still running; it reports what has failed so far.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

func (s *Session) setErr(err error) {
	s.mu.Lock()
	s.lastErr = err
	s.mu.Unlock()
}

func (s *Session) setLastRequest(msgs []Message) {
	snapshot := append([]Message{}, msgs...)
	s.mu.Lock()
	s.lastRequest = snapshot
	s.mu.Unlock()
}

// snapshotHistory copies the transcript so far, for use as the base of a new
// request's message list.
func (s *Session) snapshotHistory() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message{}, s.messages...)
}

// appendHistory adds messages to the transcript.
func (s *Session) appendHistory(msgs ...Message) {
	s.mu.Lock()
	s.messages = append(s.messages, msgs...)
	s.mu.Unlock()
}

// requestBase is what a new turn's message list is built on: the transcript so
// far, or — when Stateless is set — the system prompt alone. The system
// messages are what a model needs to behave like itself at all, so they stay
// even when everything said since is dropped.
func (s *Session) requestBase() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.Stateless {
		return append([]Message{}, s.messages...)
	}
	var out []Message
	for _, m := range s.messages {
		if m.Role == RoleSystem {
			out = append(out, m)
		}
	}
	return out
}

func (s *Session) addUsage(u Usage) {
	s.mu.Lock()
	s.usage.InputTokens += u.InputTokens
	s.usage.OutputTokens += u.OutputTokens
	s.usage.TotalTokens += u.TotalTokens
	s.usage.ReasoningTokens += u.ReasoningTokens
	s.mu.Unlock()
}

func NewSession(client *Client, systemPrompt string) *Session {
	s := &Session{
		client:              client,
		Temp:                1.0,
		Timeout:             DefaultTimeout,
		executors:           map[string]ToolExecutor{},
		SendRegisteredTools: true,
	}
	if systemPrompt != "" {
		s.messages = append(s.messages, Message{Role: RoleSystem, Content: systemPrompt})
	}
	return s
}

// AddTool registers a tool and its executor for every subsequent call on this
// session. The library ships no tools of its own; tools that touch a terminal,
// a filesystem or a network belong to the caller.
func (s *Session) AddTool(t Tool, exec ToolExecutor) {
	s.tools = append(s.tools, t)
	if s.executors == nil {
		s.executors = map[string]ToolExecutor{}
	}
	s.executors[t.Function.Name] = exec
}

// offeredTools is what goes on the wire.
func (s *Session) offeredTools() []Tool {
	if !s.SendRegisteredTools {
		return nil
	}
	return s.tools
}

// SetClient swaps the backing client, keeping the conversation history. Used
// by the interactive /model command to switch provider mid-session.
func (s *Session) SetClient(c *Client) {
	s.client = c
}

func (s *Session) Send(ctx context.Context, prompt string) (Response, error) {
	userMsg := Message{Role: RoleUser, Content: prompt}
	req := Request{
		Messages:        append(s.requestBase(), userMsg),
		Model:           s.Model,
		Temperature:     s.Temp,
		MaxTokens:       s.MaxTokens,
		ReasoningEffort: s.ReasoningEffort,
		ServiceTier:     s.ServiceTier,
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	resp, err := s.client.Chat(ctx, req)
	if err != nil {
		return Response{}, asTimeout(err)
	}
	s.appendHistory(userMsg, resp.Message)
	return resp, nil
}

func (s *Session) Stream(ctx context.Context, prompt string) (<-chan Message, error) {
	userMsg := Message{Role: RoleUser, Content: prompt}
	req := Request{
		Messages:        append(s.requestBase(), userMsg),
		Model:           s.Model,
		Temperature:     s.Temp,
		MaxTokens:       s.MaxTokens,
		ReasoningEffort: s.ReasoningEffort,
		ServiceTier:     s.ServiceTier,
	}
	innerCh, err := s.client.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	outCh := make(chan Message)
	go func() {
		defer close(outCh)
		var fullContent string
		for msg := range innerCh {
			fullContent += msg.Content
			outCh <- msg
		}
		s.appendHistory(userMsg, Message{Role: RoleAssistant, Content: fullContent})
	}()
	return outCh, nil
}

// StreamWithTools streams assistant content while supporting tool calls mid-conversation.
// It will stream the model's output, execute any tool calls using the session's executors,
// send the tool results back to the model in a follow-up streaming request, and continue
// until the model finishes without additional tool calls.
func (s *Session) StreamWithTools(ctx context.Context, prompt string) (<-chan Message, error) {
	userMsg := Message{Role: RoleUser, Content: prompt}

	s.setErr(nil)
	outCh := make(chan Message)
	go func() {
		defer close(outCh)

		// start with the request's base + user message
		base := s.requestBase()
		msgs := append(base, userMsg)

		// The transcript can no longer be recognised by its length — under
		// Stateless it is longer than the request that is being built — so
		// what has already been recorded is tracked outright. fold records
		// everything produced since the last call, which is how the transcript
		// keeps growing on turns the model never saw it.
		merged := len(base)
		fold := func(extra ...Message) {
			out := append([]Message{}, msgs[merged:]...)
			s.appendHistory(append(out, extra...)...)
			merged = len(msgs)
		}

		for {
			s.setLastRequest(msgs)
			req := Request{
				Messages:        msgs,
				Model:           s.Model,
				Temperature:     s.Temp,
				MaxTokens:       s.MaxTokens,
				Tools:           s.offeredTools(),
				ReasoningEffort: s.ReasoningEffort,
				ServiceTier:     s.ServiceTier,
			}

			// use a cancellable child context so we can stop a stream when a tool call appears
			reqCtx, cancel := context.WithCancel(ctx)
			streamCh, err := s.client.Stream(reqCtx, req)
			if err != nil {
				cancel()
				s.setErr(asTimeout(err))
				return
			}

			var fullContent string
			var sawToolCall bool

		stream:
			for {
				msg, ok := s.nextChunk(streamCh)
				if !ok {
					if s.Err() != nil {
						// Idle timeout: stop here and hand the prompt back
						// rather than waiting on a backend that went quiet.
						cancel()
						return
					}
					break stream
				}
				if msg.Err != nil {
					// A failure that arrived mid-stream. Keep whatever was
					// already streamed and hand the error to the caller.
					cancel()
					s.setErr(asTimeout(msg.Err))
					if fullContent != "" {
						fold(Message{Role: RoleAssistant, Content: fullContent})
					}
					return
				}
				// if this chunk contains a tool call, execute it and prepare for follow-up
				if len(msg.ToolCalls) > 0 {
					sawToolCall = true
					// The assistant's own tool call has to go on the transcript
					// before its results. Without it the backend receives tool
					// messages answering nothing, and asks for the same call
					// again on the follow-up request.
					msgs = append(msgs, Message{
						Role:      RoleAssistant,
						Content:   fullContent,
						ToolCalls: msg.ToolCalls,
					})
					// Tell the caller what is happening: a harness that draws
					// tool activity has no other way to see it, since these
					// messages never reach the content stream.
					select {
					case outCh <- Message{Role: RoleAssistant, ToolCalls: msg.ToolCalls}:
					case <-ctx.Done():
						cancel()
						return
					}
					// execute each tool call in order
					for _, tc := range msg.ToolCalls {
						var result string
						if fn, ok := s.executors[tc.Function.Name]; ok {
							r, err := fn(ctx, tc)
							if err != nil {
								result = fmt.Sprintf("error: %v", err)
							} else {
								result = r
							}
						} else {
							result = fmt.Sprintf("error: unknown tool %q", tc.Function.Name)
						}
						// append the tool response to msgs so the next request includes it
						toolMsg := Message{
							Role:       RoleTool,
							ToolCallID: tc.ID,
							Name:       tc.Function.Name,
							Content:    result,
						}
						msgs = append(msgs, toolMsg)
						select {
						case outCh <- toolMsg:
						case <-ctx.Done():
							cancel()
							return
						}
					}
					// we need to stop current stream and start a follow-up request
					cancel()
					break stream
				}

				if msg.Usage != nil {
					s.addUsage(*msg.Usage)
					select {
					case outCh <- msg:
					case <-ctx.Done():
						cancel()
						return
					}
					continue
				}
				// regular assistant content chunk
				if msg.Content != "" {
					fullContent += msg.Content
					select {
					case outCh <- msg:
					case <-ctx.Done():
						cancel()
						return
					}
				}
			}
			// ensure stream context cancelled
			cancel()

			// after the stream for this request finished, append assistant message
			// if any. A turn that ended in a tool call already recorded one.
			if !sawToolCall {
				msgs = append(msgs, Message{Role: RoleAssistant, Content: fullContent})
			}
			fold()

			if !sawToolCall {
				// finished without tool calls
				return
			}
			// otherwise loop will send the tool responses back and start a new streaming request
		}
	}()
	return outCh, nil
}

// asTimeout tags the transport's own timeouts as ErrTimeout, so a backend that
// never sends headers reads the same as one that goes quiet mid-stream: both
// are the provider failing, not the request being wrong.
func asTimeout(err error) error {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return fmt.Errorf("%w: %v", ErrTimeout, err)
	}
	return err
}

// withTimeout bounds a non-streaming call, where there is no output to keep
// the connection honest.
func (s *Session) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.Timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, s.Timeout)
}

// nextChunk reads one streamed message, giving up if the backend stays silent
// for longer than s.Timeout. A quiet stretch sets lastErr, so the caller can
// tell a timeout from a stream that simply ended.
func (s *Session) nextChunk(ch <-chan Message) (Message, bool) {
	if s.Timeout <= 0 {
		msg, ok := <-ch
		return msg, ok
	}
	timer := time.NewTimer(s.Timeout)
	defer timer.Stop()
	select {
	case msg, ok := <-ch:
		return msg, ok
	case <-timer.C:
		s.setErr(fmt.Errorf("%w: no output for %s", ErrTimeout, s.Timeout))
		return Message{}, false
	}
}

// History returns a copy of the conversation so far. It is a snapshot: a
// stream still running keeps appending to the session's own copy.
func (s *Session) History() []Message {
	return s.snapshotHistory()
}

func (s *Session) SendWithTools(ctx context.Context, prompt string, userTools []Tool, userExec ToolExecutor) (Response, error) {
	allTools := append(append([]Tool{}, s.offeredTools()...), userTools...)

	userMsg := Message{Role: RoleUser, Content: prompt}
	base := s.requestBase()
	msgs := append(base, userMsg)

	for {
		s.setLastRequest(msgs)
		req := Request{
			Messages:        msgs,
			Model:           s.Model,
			Temperature:     s.Temp,
			MaxTokens:       s.MaxTokens,
			Tools:           allTools,
			ReasoningEffort: s.ReasoningEffort,
			ServiceTier:     s.ServiceTier,
		}
		resp, err := s.client.Chat(ctx, req)
		if err != nil {
			return Response{}, err
		}

		s.addUsage(resp.Usage)

		if len(resp.Message.ToolCalls) == 0 {
			// Only the part of msgs this call produced: under Stateless the
			// transcript is longer than the request, and writing the request
			// back over it would throw the conversation away.
			s.appendHistory(append(msgs[len(base):], resp.Message)...)
			return resp, nil
		}

		msgs = append(msgs, resp.Message)

		for _, tc := range resp.Message.ToolCalls {
			var result string
			if fn, ok := s.executors[tc.Function.Name]; ok {
				r, err := fn(ctx, tc)
				if err != nil {
					result = fmt.Sprintf("error: %v", err)
				} else {
					result = r
				}
			} else if userExec != nil {
				r, err := userExec(ctx, tc)
				if err != nil {
					result = fmt.Sprintf("error: %v", err)
				} else {
					result = r
				}
			} else {
				result = fmt.Sprintf("error: unknown tool %q", tc.Function.Name)
			}
			msgs = append(msgs, Message{
				Role:       RoleTool,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    result,
			})
		}
	}
}

func (s *Session) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.messages) > 0 && s.messages[0].Role == RoleSystem {
		s.messages = s.messages[:1]
	} else {
		s.messages = nil
	}
}
