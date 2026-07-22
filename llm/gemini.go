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
	"sync/atomic"
	"time"
)

const defaultGeminiBaseURL = "https://generativelanguage.googleapis.com/v1beta"

type geminiProvider struct {
	apiKey   string
	model    string
	baseURL  string
	client   *http.Client
	toolID64 atomic.Int64
}

func newGeminiProvider(cfg Config) *geminiProvider {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultGeminiBaseURL
	}
	return &geminiProvider{
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 60 * time.Second},
	}
}

func (p *geminiProvider) modelName(req Request) string {
	if req.Model != "" {
		return req.Model
	}
	if p.model != "" {
		return p.model
	}
	return "gemini-2.0-flash"
}

func (p *geminiProvider) nextToolID() string {
	n := p.toolID64.Add(1)
	return fmt.Sprintf("gc-fc-%d", n)
}

// --- Gemini wire types ---

type geminiContent struct {
	Role  string          `json:"role"`
	Parts json.RawMessage `json:"parts"`
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
	ThoughtSignature string                  `json:"thoughtSignature,omitempty"`
}

type geminiFunctionCall struct {
	Name             string `json:"name"`
	Args             any    `json:"args"`
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

type geminiFunctionResponse struct {
	Name     string `json:"name"`
	Response any    `json:"response"`
}

type geminiConfig struct {
	Temperature     float64 `json:"temperature,omitempty"`
	MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFnDecl `json:"functionDeclarations"`
}

type geminiFnDecl struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters,omitempty"`
}

type geminiReq struct {
	Contents          []geminiContent `json:"contents"`
	SystemInstruction *geminiContent  `json:"systemInstruction,omitempty"`
	GenerationConfig  geminiConfig    `json:"generationConfig,omitempty"`
	Tools             []geminiTool    `json:"tools,omitempty"`
}

type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason"`
}

type geminiUsage struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

type geminiResp struct {
	Candidates []geminiCandidate `json:"candidates"`
	Usage      *geminiUsage      `json:"usageMetadata,omitempty"`
}

type geminiErr struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// --- convert our types to Gemini wire types ---

func buildGeminiReq(req Request) geminiReq {
	var contents []geminiContent
	var system *geminiContent

	part := func(ps ...geminiPart) json.RawMessage {
		b, _ := json.Marshal(ps)
		return b
	}

	for _, msg := range req.Messages {
		switch msg.Role {
		case RoleSystem:
			system = &geminiContent{Parts: part(geminiPart{Text: msg.Content})}
		case RoleUser:
			contents = append(contents, geminiContent{
				Role: "user", Parts: part(geminiPart{Text: msg.Content}),
			})
		case RoleAssistant:
			if len(msg.geminiParts) > 0 {
				contents = append(contents, geminiContent{Role: "model", Parts: msg.geminiParts})
			} else {
				var ps []geminiPart
				for _, tc := range msg.ToolCalls {
					var args any
					json.Unmarshal([]byte(tc.Function.Arguments), &args)
					ps = append(ps, geminiPart{
						FunctionCall: &geminiFunctionCall{Name: tc.Function.Name, Args: args},
					})
				}
				if msg.Content != "" {
					ps = append(ps, geminiPart{Text: msg.Content})
				}
				contents = append(contents, geminiContent{Role: "model", Parts: part(ps...)})
			}
		case RoleTool:
			var parsed any
			if err := json.Unmarshal([]byte(msg.Content), &parsed); err != nil {
				parsed = msg.Content
			}
			contents = append(contents, geminiContent{
				Role: "function",
				Parts: part(geminiPart{
					FunctionResponse: &geminiFunctionResponse{
						Name:     msg.Name,
						Response: map[string]any{"result": parsed},
					},
				}),
			})
		}
	}

	g := geminiReq{
		Contents:          contents,
		SystemInstruction: system,
		GenerationConfig: geminiConfig{
			Temperature:     req.Temperature,
			MaxOutputTokens: req.MaxTokens,
		},
	}

	if len(req.Tools) > 0 {
		var fns []geminiFnDecl
		for _, t := range req.Tools {
			if t.Type == "function" {
				fns = append(fns, geminiFnDecl{
					Name:        t.Function.Name,
					Description: t.Function.Description,
					Parameters:  t.Function.Parameters,
				})
			}
		}
		if len(fns) > 0 {
			g.Tools = []geminiTool{{FunctionDeclarations: fns}}
		}
	}

	return g
}

// --- convert Gemini response to our types ---

func geminiRespToMessage(c geminiCandidate) Message {
	msg := Message{Role: RoleAssistant}
	if len(c.Content.Parts) > 0 {
		msg.geminiParts = c.Content.Parts
	}
	var parts []geminiPart
	if err := json.Unmarshal(c.Content.Parts, &parts); err == nil {
		for _, p := range parts {
			switch {
			case p.Text != "":
				msg.Content += p.Text
			case p.FunctionCall != nil:
				args, _ := json.Marshal(p.FunctionCall.Args)
				msg.ToolCalls = append(msg.ToolCalls, ToolCall{
					ID:   "gc-fc-0",
					Type: "function",
					Function: ToolCallFunction{
						Name:      p.FunctionCall.Name,
						Arguments: string(args),
					},
				})
			}
		}
	}
	for i := range msg.ToolCalls {
		msg.ToolCalls[i].ID = fmt.Sprintf("gc-fc-%d", i)
	}
	return msg
}

// --- Chat ---

func (p *geminiProvider) Chat(ctx context.Context, req Request) (Response, error) {
	model := p.modelName(req)
	body := buildGeminiReq(req)

	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("gemini: marshal: %w", err)
	}

	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", p.baseURL, model, p.apiKey)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return Response{}, fmt.Errorf("gemini: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("gemini: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		var ge geminiErr
		if json.Unmarshal(bodyBytes, &ge) == nil && ge.Error.Message != "" {
			return Response{}, fmt.Errorf("gemini: API %d: %s", resp.StatusCode, ge.Error.Message)
		}
		return Response{}, fmt.Errorf("gemini: API %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var gr geminiResp
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return Response{}, fmt.Errorf("gemini: decode: %w", err)
	}
	if len(gr.Candidates) == 0 {
		return Response{}, errors.New("gemini: empty response")
	}

	res := Response{Message: geminiRespToMessage(gr.Candidates[0])}
	if gr.Usage != nil {
		res.Usage = Usage{
			InputTokens:  gr.Usage.PromptTokenCount,
			OutputTokens: gr.Usage.CandidatesTokenCount,
			TotalTokens:  gr.Usage.TotalTokenCount,
		}
	}
	return res, nil
}

// --- Stream ---

func (p *geminiProvider) Stream(ctx context.Context, req Request) (<-chan Message, error) {
	model := p.modelName(req)
	body := buildGeminiReq(req)

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("gemini: marshal: %w", err)
	}

	url := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse&key=%s", p.baseURL, model, p.apiKey)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("gemini: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini: http: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var ge geminiErr
		if json.Unmarshal(bodyBytes, &ge) == nil && ge.Error.Message != "" {
			return nil, fmt.Errorf("gemini: API %d: %s", resp.StatusCode, ge.Error.Message)
		}
		return nil, fmt.Errorf("gemini: API %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	ch := make(chan Message)
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

			var gr geminiResp
			if err := json.Unmarshal([]byte(data), &gr); err != nil {
				continue
			}
			if len(gr.Candidates) == 0 {
				continue
			}

			msg := geminiRespToMessage(gr.Candidates[0])
			if msg.Content != "" || len(msg.ToolCalls) > 0 {
				select {
				case ch <- msg:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch, nil
}
