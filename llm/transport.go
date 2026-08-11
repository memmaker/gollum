package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

type openaiTransport struct {
	apiKey string
	client *http.Client
	// allowServiceTier gates the OpenAI-only `service_tier` field. Only the
	// openai provider sets it; the other OpenAI-compatible backends sharing
	// this transport reject or ignore the field, so they never send it.
	allowServiceTier bool
}

type openaiReq struct {
	Model               string           `json:"model"`
	Messages            []Message        `json:"messages"`
	Temperature         float64          `json:"temperature,omitempty"`
	MaxTokens           int              `json:"max_tokens,omitempty"`
	MaxCompletionTokens int              `json:"max_completion_tokens,omitempty"`
	Stream              bool             `json:"stream"`
	Tools               []Tool           `json:"tools,omitempty"`
	ReasoningEffort     string           `json:"reasoning_effort,omitempty"`
	ServiceTier         string           `json:"service_tier,omitempty"`
	StreamOptions       *oaStreamOptions `json:"stream_options,omitempty"`
}

// oaStreamOptions asks for a final usage chunk. Without it a streamed answer
// reports no token counts at all.
type oaStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// serviceTier returns the value to put in the outgoing body, dropping it for
// backends that are OpenAI-compatible but not OpenAI.
func (t *openaiTransport) serviceTier(s ServiceTier) string {
	if !t.allowServiceTier {
		return ""
	}
	return string(s)
}

// isReasoningTierModel reports whether a model belongs to one of OpenAI's
// reasoning-tier families (o1, o3, o4-mini, gpt-5). These reject several
// chat-completion parameters that older models accept without complaint:
// max_tokens (must use max_completion_tokens instead) and any non-default
// temperature. Everything else - the gpt-4 family, local OpenAI-compatible
// servers served through lmstudio, and Copilot's own model aliases - still
// accepts the legacy parameter set.
func isReasoningTierModel(model string) bool {
	for _, prefix := range []string{"o1", "o3", "o4", "gpt-5"} {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

// maxTokenFields returns the (max_tokens, max_completion_tokens) pair to set
// on an outgoing request body, routing the caller's token budget to whichever
// parameter the target model accepts.
func maxTokenFields(model string, maxTokens int) (legacy int, completion int) {
	if isReasoningTierModel(model) {
		return 0, maxTokens
	}
	return maxTokens, 0
}

// effectiveTemperature returns the temperature to send for a model.
// Reasoning-tier models only support their default temperature (1) and
// reject any override with a 400, so the field is omitted (the zero value
// is dropped by the request struct's omitempty tag) rather than sent.
func effectiveTemperature(model string, requested float64) float64 {
	if isReasoningTierModel(model) {
		return 0
	}
	return requested
}

// openaiReasoningEffort maps our provider-agnostic enum to the values the
// target model accepts. OpenAI's own reasoning-tier models have no literal
// "off", so None maps to "minimal" there; other OpenAI-compatible backends
// (local proxies, lmstudio, etc.) take the literal "none", which actually
// suppresses reasoning_content instead of just minimizing it.
func openaiReasoningEffort(e ReasoningEffort, model string) string {
	// The gpt-5*-chat variants carry a reasoning-tier name but accept only
	// their own default effort and 400 on every other value, so send nothing.
	if strings.Contains(model, "-chat") {
		return ""
	}
	switch e {
	case ReasoningEffortNone:
		if isReasoningTierModel(model) {
			return "minimal"
		}
		return "none"
	case ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh:
		return string(e)
	default:
		return ""
	}
}

type openaiResp struct {
	ID      string     `json:"id"`
	Object  string     `json:"object"`
	Choices []oaChoice `json:"choices"`
	Usage   *oaUsage   `json:"usage,omitempty"`
}

type oaChoice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type oaUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	Details          *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
}

func (u *oaUsage) toUsage() Usage {
	res := Usage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
		TotalTokens:  u.TotalTokens,
	}
	if u.Details != nil {
		res.ReasoningTokens = u.Details.ReasoningTokens
	}
	return res
}

type oaChunk struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Choices []oaChunkChoice `json:"choices"`
	Usage   *oaUsage        `json:"usage,omitempty"`
}

type oaChunkChoice struct {
	Index        int     `json:"index"`
	Delta        oaDelta `json:"delta"`
	FinishReason string  `json:"finish_reason,omitempty"`
}

type oaDelta struct {
	Role         string          `json:"role,omitempty"`
	Content      string          `json:"content,omitempty"`
	ToolCalls    []oaToolCall    `json:"tool_calls,omitempty"`
	FunctionCall *oaFunctionCall `json:"function_call,omitempty"`
}

type oaToolCall struct {
	Index    int                `json:"index,omitempty"`
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function oaToolCallFunction `json:"function,omitempty"`
}

type oaToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type oaFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// headerTimeoutTransport bounds the wait for response headers without bounding
// the response body: a streamed answer may legitimately take minutes, but a
// backend that accepts the connection and never replies must not hang the
// client. Client.Timeout cannot express that — it covers the body read too.
func headerTimeoutTransport(d time.Duration) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = d
	return t
}

func newOpenAITransport(apiKey string, timeout time.Duration) *openaiTransport {
	return &openaiTransport{
		apiKey: apiKey,
		client: &http.Client{Transport: headerTimeoutTransport(timeout)},
	}
}

func (t *openaiTransport) chat(ctx context.Context, url string, req Request) (Response, error) {
	legacyMax, completionMax := maxTokenFields(req.Model, req.MaxTokens)
	body := openaiReq{
		Model:               req.Model,
		Messages:            req.Messages,
		Temperature:         effectiveTemperature(req.Model, req.Temperature),
		MaxTokens:           legacyMax,
		MaxCompletionTokens: completionMax,
		Tools:               req.Tools,
		ReasoningEffort:     openaiReasoningEffort(req.ReasoningEffort, req.Model),
		ServiceTier:         t.serviceTier(req.ServiceTier),
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("llm: marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return Response{}, fmt.Errorf("llm: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+t.apiKey)

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("llm: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return Response{}, fmt.Errorf("llm: API %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var oa openaiResp
	if err := json.NewDecoder(resp.Body).Decode(&oa); err != nil {
		return Response{}, fmt.Errorf("llm: decode: %w", err)
	}
	if len(oa.Choices) == 0 {
		return Response{}, errors.New("llm: empty response")
	}

	res := Response{Message: oa.Choices[0].Message}
	if oa.Usage != nil {
		res.Usage = oa.Usage.toUsage()
	}
	return res, nil
}

func (t *openaiTransport) stream(ctx context.Context, url string, req Request) (<-chan Message, error) {
	legacyMax, completionMax := maxTokenFields(req.Model, req.MaxTokens)
	body := openaiReq{
		Model:               req.Model,
		Messages:            req.Messages,
		Temperature:         effectiveTemperature(req.Model, req.Temperature),
		MaxTokens:           legacyMax,
		MaxCompletionTokens: completionMax,
		Stream:              true,
		Tools:               req.Tools,
		ReasoningEffort:     openaiReasoningEffort(req.ReasoningEffort, req.Model),
		ServiceTier:         t.serviceTier(req.ServiceTier),
		StreamOptions:       &oaStreamOptions{IncludeUsage: true},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("llm: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+t.apiKey)

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llm: http: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("llm: API %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	ch := make(chan Message)
	// state per choice index: accumulate role, content, function call name/args, tool calls
	type partial struct {
		role      string
		content   strings.Builder
		fnName    string
		fnArgs    strings.Builder
		toolCalls map[int]*oaToolCall
	}
	parts := make(map[int]*partial)
	var lastUsage *Usage

	// helper to ensure partial exists
	getPart := func(idx int) *partial {
		if p, ok := parts[idx]; ok {
			return p
		}
		p := &partial{}
		parts[idx] = p
		return p
	}

	go func() {
		defer resp.Body.Close()
		defer close(ch)

		fail := func(err error) {
			select {
			case ch <- Message{Role: RoleAssistant, Err: err}:
			case <-ctx.Done():
			}
		}

		scanner := bufio.NewScanner(resp.Body)
		// SSE chunks can exceed bufio's default 64KB line limit.
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	scan:
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break scan
			}

			// Routers report mid-stream failures as an error object in the
			// SSE body rather than an HTTP status, so look for one first.
			var errPayload struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
					Code    any    `json:"code"`
				} `json:"error"`
			}
			if json.Unmarshal([]byte(data), &errPayload) == nil && errPayload.Error.Message != "" {
				fail(fmt.Errorf("llm: backend error: %s", describeAPIError(errPayload.Error.Message, errPayload.Error.Type, errPayload.Error.Code)))
				return
			}

			var chunk oaChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				fail(fmt.Errorf("llm: unreadable chunk: %w: %s", err, truncateForError(data)))
				return
			}
			// Usage is meant to arrive once, in a final choice-less chunk, but
			// some routers (e.g. free OpenRouter models) attach a running
			// total to every chunk. Keep only the latest and emit it once the
			// stream ends, or a duplicate lands per token instead of per turn.
			if chunk.Usage != nil {
				u := chunk.Usage.toUsage()
				lastUsage = &u
			}
			for _, c := range chunk.Choices {
				p := getPart(c.Index)
				// role
				if c.Delta.Role != "" {
					p.role = c.Delta.Role
				}
				// content
				if c.Delta.Content != "" {
					// emit incremental content
					text := c.Delta.Content
					p.content.WriteString(text)
					select {
					case ch <- Message{Role: RoleAssistant, Content: text}:
					case <-ctx.Done():
						return
					}
				}
				// new tool_calls format (OpenAI November 2024+)
				if len(c.Delta.ToolCalls) > 0 {
					if p.toolCalls == nil {
						p.toolCalls = make(map[int]*oaToolCall)
					}
					for _, tc := range c.Delta.ToolCalls {
						idx := tc.Index
						if _, exists := p.toolCalls[idx]; !exists {
							p.toolCalls[idx] = &oaToolCall{
								Index: idx,
								ID:    tc.ID,
								Type:  tc.Type,
							}
						}
						if tc.Function.Name != "" {
							p.toolCalls[idx].Function.Name = tc.Function.Name
						}
						if tc.Function.Arguments != "" {
							p.toolCalls[idx].Function.Arguments += tc.Function.Arguments
						}
					}
				}
				// legacy function_call format (deprecated but still supported)
				if c.Delta.FunctionCall != nil {
					if c.Delta.FunctionCall.Name != "" {
						p.fnName = c.Delta.FunctionCall.Name
					}
					if c.Delta.FunctionCall.Arguments != "" {
						p.fnArgs.WriteString(c.Delta.FunctionCall.Arguments)
					}
				}
				// finish reason: if tool_call/function_call completed, emit a tool-call message
				if c.FinishReason != "" {
					// Handle new tool_calls format
					if c.FinishReason == "tool_calls" && p.toolCalls != nil {
						// Deltas are accumulated by index, and ranging a map
						// would hand the caller the calls in random order.
						// Callers rely on the model's own ordering, so replay
						// it by index.
						indices := make([]int, 0, len(p.toolCalls))
						for idx := range p.toolCalls {
							indices = append(indices, idx)
						}
						sort.Ints(indices)

						var toolCalls []ToolCall
						for _, idx := range indices {
							tc := p.toolCalls[idx]
							toolCalls = append(toolCalls, ToolCall{
								ID:   tc.ID,
								Type: tc.Type,
								Function: ToolCallFunction{
									Name:      tc.Function.Name,
									Arguments: tc.Function.Arguments,
								},
							})
						}
						if len(toolCalls) > 0 {
							select {
							case ch <- Message{Role: RoleAssistant, ToolCalls: toolCalls}:
							case <-ctx.Done():
								return
							}
						}
						delete(parts, c.Index)
					} else if (c.FinishReason == "function_call" || p.fnName != "") && c.FinishReason != "tool_calls" {
						// Handle legacy function_call format
						tc := ToolCall{
							ID:   fmt.Sprintf("oa-fc-%d", c.Index),
							Type: "function",
							Function: ToolCallFunction{
								Name:      p.fnName,
								Arguments: p.fnArgs.String(),
							},
						}
						select {
						case ch <- Message{Role: RoleAssistant, ToolCalls: []ToolCall{tc}}:
						case <-ctx.Done():
							return
						}
						delete(parts, c.Index)
					}
				}
			}
		}
		if err := scanner.Err(); err != nil {
			// The stream stopped early: a dropped connection, a proxy cutting
			// the response. Silence here reads as a complete empty answer.
			fail(fmt.Errorf("llm: stream ended early: %w", err))
			return
		}
		if lastUsage != nil {
			select {
			case ch <- Message{Role: RoleAssistant, Usage: lastUsage}:
			case <-ctx.Done():
			}
		}
	}()
	return ch, nil
}

// describeAPIError keeps whatever the backend chose to tell us.
func describeAPIError(message, kind string, code any) string {
	out := message
	if kind != "" {
		out += " (" + kind
		if code != nil && code != "" {
			out += fmt.Sprintf(", code %v", code)
		}
		out += ")"
	} else if code != nil && code != "" {
		out += fmt.Sprintf(" (code %v)", code)
	}
	return out
}

// truncateForError keeps an unparsable payload short enough to read.
func truncateForError(s string) string {
	const limit = 200
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
