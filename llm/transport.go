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
	"strings"
)

type openaiTransport struct {
	apiKey string
	client *http.Client
}

type openaiReq struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
	Tools       []Tool    `json:"tools,omitempty"`
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
}

type oaChunk struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Choices []oaChunkChoice `json:"choices"`
}

type oaChunkChoice struct {
	Index        int     `json:"index"`
	Delta        oaDelta `json:"delta"`
	FinishReason string  `json:"finish_reason,omitempty"`
}

type oaDelta struct {
	Role         string          `json:"role,omitempty"`
	Content      string          `json:"content,omitempty"`
	FunctionCall *oaFunctionCall `json:"function_call,omitempty"`
}

type oaFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

func newOpenAITransport(apiKey string) *openaiTransport {
	return &openaiTransport{
		apiKey: apiKey,
		client: &http.Client{},
	}
}

func (t *openaiTransport) chat(ctx context.Context, url string, req Request) (Response, error) {
	body := openaiReq{
		Model:       req.Model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Tools:       req.Tools,
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
		res.Usage = Usage{
			InputTokens:  oa.Usage.PromptTokens,
			OutputTokens: oa.Usage.CompletionTokens,
			TotalTokens:  oa.Usage.TotalTokens,
		}
	}
	return res, nil
}

func (t *openaiTransport) stream(ctx context.Context, url string, req Request) (<-chan Message, error) {
	body := openaiReq{
		Model:       req.Model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      true,
		Tools:       req.Tools,
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
	// state per choice index: accumulate role, content, function call name/args
	type partial struct {
		role    string
		content strings.Builder
		fnName  string
		fnArgs  strings.Builder
	}
	parts := make(map[int]*partial)

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

		scanner := bufio.NewScanner(resp.Body)
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
				return
			}

			var chunk oaChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
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
				// function call pieces
				if c.Delta.FunctionCall != nil {
					if c.Delta.FunctionCall.Name != "" {
						p.fnName = c.Delta.FunctionCall.Name
					}
					if c.Delta.FunctionCall.Arguments != "" {
						p.fnArgs.WriteString(c.Delta.FunctionCall.Arguments)
					}
				}
				// finish reason: if function_call completed, emit a tool-call message
				if c.FinishReason != "" {
					if c.FinishReason == "function_call" || p.fnName != "" {
						// assemble ToolCall
						tc := ToolCall{
							ID:   fmt.Sprintf("oa-fc-%d", c.Index),
							Type: "function",
							Function: ToolCallFunction{
								Name:      p.fnName,
								Arguments: p.fnArgs.String(),
							},
						}
						// send a message that contains the tool call (no content)
						select {
						case ch <- Message{Role: RoleAssistant, ToolCalls: []ToolCall{tc}}:
						case <-ctx.Done():
							return
						}
						// reset partial for this index to free memory
						delete(parts, c.Index)
					}
				}
			}
		}
	}()
	return ch, nil
}
