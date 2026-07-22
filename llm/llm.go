package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`

	geminiParts json.RawMessage `json:"-"` // Gemini: raw JSON of response parts, echoed verbatim
}

type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

type Request struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Tools       []Tool    `json:"tools,omitempty"`
}

type Response struct {
	Message Message
	Usage   Usage
}

type Usage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	TotalTokens  int `json:"total_tokens,omitempty"`
}

type Provider interface {
	Chat(ctx context.Context, req Request) (Response, error)
	Stream(ctx context.Context, req Request) (<-chan Message, error)
}

type Config struct {
	Provider string `yaml:"provider"`
	APIKey   string `yaml:"api_key"`
	Model    string `yaml:"model"`
	BaseURL  string `yaml:"base_url"`
}

type Client struct {
	provider Provider
}

func New(cfg Config) (*Client, error) {
	var p Provider
	switch cfg.Provider {
	case "copilot":
		p = newCopilotProvider(cfg)
	case "huggingface":
		p = newHuggingFaceProvider(cfg)
	case "openai":
		p = newOpenAIProvider(cfg)
	case "gemini":
		p = newGeminiProvider(cfg)
	default:
		return nil, fmt.Errorf("llm: unknown provider %q", cfg.Provider)
	}
	return &Client{provider: p}, nil
}

func NewFromEnv() (*Client, error) {
	// Prefer OpenAI if an API key is available
	if key := env("OPENAI_API_KEY", "OPENAI_API_TOKEN"); key != "" {
		return New(Config{Provider: "openai", APIKey: key})
	}
	if key := env("HUGGINGFACE_API_KEY", "HF_API_KEY"); key != "" {
		return New(Config{Provider: "huggingface", APIKey: key})
	}
	if key := env("GITHUB_TOKEN", "COPILOT_TOKEN"); key != "" {
		return New(Config{Provider: "copilot", APIKey: key})
	}
	if key := env("GEMINI_API_KEY", "GOOGLE_API_KEY"); key != "" {
		return New(Config{Provider: "gemini", APIKey: key})
	}
	if token, err := exec.Command("gh", "auth", "token").Output(); err == nil {
		return New(Config{Provider: "copilot", APIKey: strings.TrimSpace(string(token))})
	}
	return nil, errors.New("llm: no API key found")
}

func (c *Client) Chat(ctx context.Context, req Request) (Response, error) {
	return c.provider.Chat(ctx, req)
}

func (c *Client) Stream(ctx context.Context, req Request) (<-chan Message, error) {
	return c.provider.Stream(ctx, req)
}

func env(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
