package llm

import (
	"context"
	"fmt"
)

type ToolExecutor func(ctx context.Context, call ToolCall) (string, error)

type Session struct {
	client    *Client
	messages  []Message
	Model     string
	Temp      float64
	MaxTokens int
	tools     []Tool
	executors map[string]ToolExecutor
}

func NewSession(client *Client, systemPrompt string) *Session {
	s := &Session{
		client:    client,
		Temp:      1.0,
		tools:     builtinTools(),
		executors: builtinExecutors(),
	}
	if systemPrompt != "" {
		s.messages = append(s.messages, Message{Role: RoleSystem, Content: systemPrompt})
	}
	return s
}

func (s *Session) Send(ctx context.Context, prompt string) (Response, error) {
	userMsg := Message{Role: RoleUser, Content: prompt}
	req := Request{
		Messages:    append(append([]Message{}, s.messages...), userMsg),
		Model:       s.Model,
		Temperature: s.Temp,
		MaxTokens:   s.MaxTokens,
	}
	resp, err := s.client.Chat(ctx, req)
	if err != nil {
		return Response{}, err
	}
	s.messages = append(s.messages, userMsg, resp.Message)
	return resp, nil
}

func (s *Session) Stream(ctx context.Context, prompt string) (<-chan Message, error) {
	userMsg := Message{Role: RoleUser, Content: prompt}
	req := Request{
		Messages:    append(append([]Message{}, s.messages...), userMsg),
		Model:       s.Model,
		Temperature: s.Temp,
		MaxTokens:   s.MaxTokens,
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
		s.messages = append(s.messages, userMsg, Message{Role: RoleAssistant, Content: fullContent})
	}()
	return outCh, nil
}

// StreamWithTools streams assistant content while supporting tool calls mid-conversation.
// It will stream the model's output, execute any tool calls using the session's executors,
// send the tool results back to the model in a follow-up streaming request, and continue
// until the model finishes without additional tool calls.
func (s *Session) StreamWithTools(ctx context.Context, prompt string) (<-chan Message, error) {
	userMsg := Message{Role: RoleUser, Content: prompt}

	outCh := make(chan Message)
	go func() {
		defer close(outCh)

		// start with messages + user message
		msgs := append(append([]Message{}, s.messages...), userMsg)

		for {
			req := Request{
				Messages:    msgs,
				Model:       s.Model,
				Temperature: s.Temp,
				MaxTokens:   s.MaxTokens,
				Tools:       s.tools,
			}

			// use a cancellable child context so we can stop a stream when a tool call appears
			reqCtx, cancel := context.WithCancel(ctx)
			streamCh, err := s.client.Stream(reqCtx, req)
			if err != nil {
				cancel()
				// send an error message on outCh? instead just return
				return
			}

			var fullContent string
			var sawToolCall bool

			for msg := range streamCh {
				// if this chunk contains a tool call, execute it and prepare for follow-up
				if len(msg.ToolCalls) > 0 {
					sawToolCall = true
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
						msgs = append(msgs, Message{
							Role:       RoleTool,
							ToolCallID: tc.ID,
							Name:       tc.Function.Name,
							Content:    result,
						})
					}
					// we need to stop current stream and start a follow-up request
					cancel()
					break
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

			// after the stream for this request finished, append assistant message if any
			msgs = append(msgs, Message{Role: RoleAssistant, Content: fullContent})
			s.messages = append(s.messages, msgs[len(s.messages):]...)

			if !sawToolCall {
				// finished without tool calls
				return
			}
			// otherwise loop will send the tool responses back and start a new streaming request
		}
	}()
	return outCh, nil
}

func (s *Session) History() []Message {
	return s.messages
}

func (s *Session) SendWithTools(ctx context.Context, prompt string, userTools []Tool, userExec ToolExecutor) (Response, error) {
	allTools := append(append([]Tool{}, s.tools...), userTools...)

	userMsg := Message{Role: RoleUser, Content: prompt}
	msgs := append(append([]Message{}, s.messages...), userMsg)

	for {
		req := Request{
			Messages:    msgs,
			Model:       s.Model,
			Temperature: s.Temp,
			MaxTokens:   s.MaxTokens,
			Tools:       allTools,
		}
		resp, err := s.client.Chat(ctx, req)
		if err != nil {
			return Response{}, err
		}

		if len(resp.Message.ToolCalls) == 0 {
			s.messages = append(msgs, resp.Message)
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
	if len(s.messages) > 0 && s.messages[0].Role == RoleSystem {
		s.messages = s.messages[:1]
	} else {
		s.messages = nil
	}
}
