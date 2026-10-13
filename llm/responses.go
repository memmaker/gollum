package llm

// The Responses API (POST /v1/responses). It reaches the same models as
// chat/completions over a different wire format, which would not be worth a
// second transport except that some models accept combinations there that
// chat/completions refuses outright:
//
//	Function tools with reasoning_effort are not supported for gpt-5.6-luna
//	in /v1/chat/completions. To use function tools, use /v1/responses or set
//	reasoning_effort to 'none'.
//
// A tool-calling agent that also wants the model to think has no way around
// that on the older endpoint, and "reasoning effort" is a correctness setting
// for such an agent, not a speed knob. So: same Provider interface, same
// Message types, different envelope.
//
// The differences that matter, all confirmed against the live API:
//   - `input` replaces `messages`, and its items are a union rather than one
//     shape — a role/content message, a `function_call`, or a
//     `function_call_output`. Tool results are linked by `call_id`, which is
//     what `tool_call_id` becomes.
//   - tools are flat (`{type, name, parameters}`), not nested under `function`.
//   - `reasoning: {effort}` replaces `reasoning_effort`.
//   - the answer is an `output` array of typed items rather than a choice with
//     one message, so text and tool calls arrive side by side.
//   - usage is already named the way this package names it.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type responsesProvider struct {
	transport *responsesTransport
	model     string
	baseURL   string
}

// newResponsesProvider builds a provider for a backend speaking OpenAI's
// Responses API. allowServiceTier is OpenAI-only, exactly as in
// newOpenAICompatProvider.
func newResponsesProvider(cfg Config, defaultURL string, allowServiceTier bool) *responsesProvider {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultURL
	}
	return &responsesProvider{
		transport: &responsesTransport{
			apiKey:           cfg.APIKey,
			client:           &http.Client{Transport: headerTimeoutTransport(cfg.timeout())},
			allowServiceTier: allowServiceTier,
		},
		model:   cfg.Model,
		baseURL: baseURL,
	}
}

func (p *responsesProvider) SetTimeout(d time.Duration) { p.transport.setTimeout(d) }

func (p *responsesProvider) url() string { return p.baseURL + "/responses" }

func (p *responsesProvider) Chat(ctx context.Context, req Request) (Response, error) {
	if req.Model == "" {
		req.Model = p.model
	}
	return p.transport.chat(ctx, p.url(), req)
}

func (p *responsesProvider) Stream(ctx context.Context, req Request) (<-chan Message, error) {
	if req.Model == "" {
		req.Model = p.model
	}
	return p.transport.stream(ctx, p.url(), req)
}

type responsesTransport struct {
	apiKey string
	client *http.Client
	// allowServiceTier gates OpenAI's own service_tier field — see
	// openaiTransport.allowServiceTier, whose rule this mirrors.
	allowServiceTier bool
}

func (t *responsesTransport) setTimeout(d time.Duration) { setHeaderTimeout(t.client, d) }

func (t *responsesTransport) serviceTier(s ServiceTier, model string) string {
	if !t.allowServiceTier {
		return ""
	}
	if vendor, _, ok := strings.Cut(model, "/"); ok && vendor != "openai" {
		return ""
	}
	return string(s)
}

// --- request ----------------------------------------------------------------

type responsesReq struct {
	Model           string           `json:"model"`
	Input           []any            `json:"input"`
	Tools           []responsesTool  `json:"tools,omitempty"`
	ToolChoice      string           `json:"tool_choice,omitempty"`
	Reasoning       *responsesEffort `json:"reasoning,omitempty"`
	ServiceTier     string           `json:"service_tier,omitempty"`
	Temperature     float64          `json:"temperature,omitempty"`
	MaxOutputTokens int              `json:"max_output_tokens,omitempty"`
	Stream          bool             `json:"stream,omitempty"`
	// Store is sent explicitly false rather than left to the account default:
	// this package is a library in someone else's program, and keeping a copy
	// of every prompt on the provider's side is not a decision it should make
	// silently on the caller's behalf.
	Store bool `json:"store"`
}

type responsesEffort struct {
	Effort string `json:"effort"`
}

type responsesTool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
	// Strict is sent explicitly false. The Responses API defaults function
	// tools to strict schema validation, which additionally demands that every
	// property be listed in `required` and that `additionalProperties` be
	// false. Callers write their schemas against chat/completions' rules, so
	// enabling that here would reject tools that work on the other endpoint.
	Strict bool `json:"strict"`
}

// The three shapes an input item can take. Kept as separate types rather than
// one struct with omitempty because they are a union on the wire, and an empty
// field that happens to be legal in one shape is a rejected extra key in
// another.
type responsesMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responsesFunctionCall struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type responsesFunctionOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// responsesInput flattens a conversation into the API's input array. One
// Message can become several items: an assistant turn that both said something
// and called two tools is three.
func responsesInput(messages []Message) []any {
	items := make([]any, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case RoleTool:
			items = append(items, responsesFunctionOutput{
				Type: "function_call_output", CallID: m.ToolCallID, Output: m.Content,
			})
		case RoleAssistant:
			if m.Content != "" {
				items = append(items, responsesMessage{Role: RoleAssistant, Content: m.Content})
			}
			for _, c := range m.ToolCalls {
				args := c.Function.Arguments
				if strings.TrimSpace(args) == "" {
					// The field is required and must parse as JSON. A model
					// that called a no-argument tool sometimes sends nothing.
					args = "{}"
				}
				items = append(items, responsesFunctionCall{
					Type: "function_call", CallID: c.ID, Name: c.Function.Name, Arguments: args,
				})
			}
		default:
			items = append(items, responsesMessage{Role: m.Role, Content: m.Content})
		}
	}
	return items
}

func responsesTools(tools []Tool) []responsesTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]responsesTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, responsesTool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	return out
}

func (t *responsesTransport) body(req Request, stream bool) responsesReq {
	body := responsesReq{
		Model:           req.Model,
		Input:           responsesInput(req.Messages),
		Tools:           responsesTools(req.Tools),
		ToolChoice:      toolChoice(req.ToolChoice, req.Tools),
		ServiceTier:     t.serviceTier(req.ServiceTier, req.Model),
		Temperature:     effectiveTemperature(req.Model, req.Temperature),
		MaxOutputTokens: req.MaxTokens,
		Stream:          stream,
		Store:           false,
	}
	// The effort names are the same as chat/completions', including the
	// per-model quirks openaiReasoningEffort already knows about; only the
	// envelope differs.
	if effort := openaiReasoningEffort(req.ReasoningEffort, req.Model); effort != "" {
		body.Reasoning = &responsesEffort{Effort: effort}
	}
	return body
}

func (t *responsesTransport) post(ctx context.Context, url string, body responsesReq) (*http.Response, error) {
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
	return resp, nil
}

// --- response ---------------------------------------------------------------

type responsesResp struct {
	Status string `json:"status"`
	Error  *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []responsesOutputItem `json:"output"`
	Usage  *responsesUsage       `json:"usage"`
}

type responsesOutputItem struct {
	Type string `json:"type"`
	// message
	Content []responsesContentPart `json:"content"`
	// function_call
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	// reasoning
	Summary []responsesContentPart `json:"summary"`
}

type responsesContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	TotalTokens         int `json:"total_tokens"`
	OutputTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u *responsesUsage) toUsage() Usage {
	res := Usage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  u.TotalTokens,
	}
	if u.OutputTokensDetails != nil {
		res.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
	}
	return res
}

// message folds the output array into one assistant Message. Text parts are
// concatenated, function calls are kept in the order the model emitted them,
// and a reasoning summary — when the request asked for one — lands in
// Reasoning, which is never sent back.
func (r responsesResp) message() Message {
	var text, reasoning strings.Builder
	msg := Message{Role: RoleAssistant}
	for _, item := range r.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					text.WriteString(part.Text)
				}
			}
		case "function_call":
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:       item.CallID,
				Type:     "function",
				Function: ToolCallFunction{Name: item.Name, Arguments: item.Arguments},
			})
		case "reasoning":
			for _, part := range item.Summary {
				reasoning.WriteString(part.Text)
			}
		}
	}
	msg.Content = text.String()
	msg.Reasoning = reasoning.String()
	return msg
}

// failure reports why a response carries no usable answer. A Responses call
// can come back HTTP 200 with status "failed" or "incomplete", which the
// status code alone would read as success.
func (r responsesResp) failure() error {
	if r.Error != nil && r.Error.Message != "" {
		return fmt.Errorf("llm: backend error: %s", describeAPIError(r.Error.Message, r.Error.Type, r.Error.Code))
	}
	switch r.Status {
	case "failed":
		return fmt.Errorf("llm: response failed")
	case "incomplete":
		reason := "unknown reason"
		if r.IncompleteDetails != nil && r.IncompleteDetails.Reason != "" {
			reason = r.IncompleteDetails.Reason
		}
		return fmt.Errorf("llm: response incomplete: %s", reason)
	}
	return nil
}

func (t *responsesTransport) chat(ctx context.Context, url string, req Request) (Response, error) {
	resp, err := t.post(ctx, url, t.body(req, false))
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	var r responsesResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return Response{}, fmt.Errorf("llm: decode: %w", err)
	}
	if err := r.failure(); err != nil {
		return Response{}, err
	}

	res := Response{Message: r.message()}
	if r.Usage != nil {
		res.Usage = r.Usage.toUsage()
	}
	return res, nil
}

// --- streaming --------------------------------------------------------------

// responsesEvent is one SSE event. Text arrives as deltas; everything else is
// read off the terminal event, which carries the whole finished response —
// so tool-call arguments never have to be reassembled from fragments the way
// chat/completions requires.
type responsesEvent struct {
	Type     string         `json:"type"`
	Delta    string         `json:"delta"`
	Response *responsesResp `json:"response"`
	Error    *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

func (t *responsesTransport) stream(ctx context.Context, url string, req Request) (<-chan Message, error) {
	resp, err := t.post(ctx, url, t.body(req, true))
	if err != nil {
		return nil, err
	}

	ch := make(chan Message)
	go func() {
		defer resp.Body.Close()
		defer close(ch)

		send := func(m Message) bool {
			select {
			case ch <- m:
				return true
			case <-ctx.Done():
				return false
			}
		}
		fail := func(err error) { send(Message{Role: RoleAssistant, Err: err}) }

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}

			var event responsesEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				fail(fmt.Errorf("llm: unreadable chunk: %w: %s", err, truncateForError(data)))
				return
			}

			switch event.Type {
			case "response.output_text.delta":
				if event.Delta != "" && !send(Message{Role: RoleAssistant, Content: event.Delta}) {
					return
				}
			case "response.reasoning_summary_text.delta":
				if event.Delta != "" && !send(Message{Role: RoleAssistant, Reasoning: event.Delta}) {
					return
				}
			case "error", "response.failed", "response.incomplete":
				if event.Error != nil && event.Error.Message != "" {
					fail(fmt.Errorf("llm: backend error: %s",
						describeAPIError(event.Error.Message, event.Error.Type, event.Error.Code)))
					return
				}
				if event.Response != nil {
					if err := event.Response.failure(); err != nil {
						fail(err)
						return
					}
				}
				fail(fmt.Errorf("llm: stream reported %s", event.Type))
				return
			case "response.completed":
				if event.Response == nil {
					continue
				}
				// The text was already streamed delta by delta; only the tool
				// calls and the usage are still owed to the caller.
				if msg := event.Response.message(); len(msg.ToolCalls) > 0 {
					if !send(Message{Role: RoleAssistant, ToolCalls: msg.ToolCalls}) {
						return
					}
				}
				if event.Response.Usage != nil {
					u := event.Response.Usage.toUsage()
					send(Message{Role: RoleAssistant, Usage: &u})
				}
				return
			}
		}
		if err := scanner.Err(); err != nil {
			fail(fmt.Errorf("llm: stream ended early: %w", err))
		}
	}()
	return ch, nil
}
