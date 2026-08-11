package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
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

	// Usage is set on the final streamed message when the backend reports
	// token counts. Never sent back to a provider.
	Usage *Usage `json:"-"`

	// Err carries a failure that happened after streaming began — a dropped
	// connection, an error object in the SSE body, unreadable chunks. Before
	// this existed such failures ended the stream silently and looked like an
	// empty answer, which is impossible to diagnose from the client side.
	Err error `json:"-"`

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

// ReasoningEffort selects how much internal reasoning a model should do
// before answering. Support and exact meaning vary by provider/model:
// OpenAI-compatible backends (openai, copilot, lmstudio, huggingface) map
// this to the `reasoning_effort` field; Gemini maps it to a thinking token
// budget. Leave it empty to use the provider/model default.
type ReasoningEffort string

const (
	ReasoningEffortNone   ReasoningEffort = "none"
	ReasoningEffortLow    ReasoningEffort = "low"
	ReasoningEffortMedium ReasoningEffort = "medium"
	ReasoningEffortHigh   ReasoningEffort = "high"
)

// ServiceTier selects OpenAI's processing tier for a request. Flex trades
// higher latency for lower cost. It is OpenAI-only: the field is dropped for
// every other provider, including the OpenAI-compatible ones (copilot,
// lmstudio, huggingface). Leave it empty for the account default.
type ServiceTier string

const ServiceTierFlex ServiceTier = "flex"

type Request struct {
	Model           string          `json:"model"`
	Messages        []Message       `json:"messages"`
	Temperature     float64         `json:"temperature,omitempty"`
	MaxTokens       int             `json:"max_tokens,omitempty"`
	Tools           []Tool          `json:"tools,omitempty"`
	ReasoningEffort ReasoningEffort `json:"reasoning_effort,omitempty"`
	ServiceTier     ServiceTier     `json:"service_tier,omitempty"`
}

type Response struct {
	Message Message
	Usage   Usage
}

type Usage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	TotalTokens  int `json:"total_tokens,omitempty"`
	// ReasoningTokens is the thinking part of OutputTokens, not an addition to
	// it. Backends that report none leave it zero.
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
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
	// Timeout bounds the wait for response headers — the phase before any
	// output exists, which no per-chunk timer can cover. Zero means
	// DefaultTimeout.
	Timeout time.Duration `yaml:"timeout"`
}

func (c Config) timeout() time.Duration {
	if c.Timeout <= 0 {
		return DefaultTimeout
	}
	return c.Timeout
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
	case "lmstudio", "omniroute":
		// Local OpenAI-compatible gateways: same transport, no API key, the
		// base_url from the config decides where requests go.
		p = newLMStudioProvider(cfg)
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
