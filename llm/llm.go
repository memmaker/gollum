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

	// Reasoning is a provider's own thought-summary text, when it sends one —
	// currently only Gemini, with thinkingConfig.includeThoughts on. Never
	// sent back to a provider: it's not part of the conversation, just a
	// window into it.
	Reasoning string `json:"-"`

	// Usage is set on the final streamed message when the backend reports
	// token counts. Never sent back to a provider.
	Usage *Usage `json:"-"`

	// Compacted marks the notice the session emits when it summarized the
	// conversation mid-turn to keep the next request inside the context
	// window. Content carries the summary, or Err says why there is none.
	// Never sent back to a provider.
	Compacted bool `json:"-"`

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

	// ollama's own validator takes four more names than OpenAI does, and a
	// model's chat template decides which of them it actually implements —
	// a qwen3 build treats xhigh as its default and has no "high" at all.
	// Only ollama is offered these.
	ReasoningEffortMinimal ReasoningEffort = "minimal"
	ReasoningEffortXHigh   ReasoningEffort = "xhigh"
	ReasoningEffortUltra   ReasoningEffort = "ultra"
	ReasoningEffortMax     ReasoningEffort = "max"
)

// ReasoningChoices lists the ReasoningEffort values a given provider/model
// combination actually respects, lightest first. Empty means the request
// carries no reasoning control worth showing the user: a provider this
// package has no reasoning wiring for, or an OpenAI "-chat" model variant,
// which 400s on anything but its own default effort.
func ReasoningChoices(provider, model string) []ReasoningEffort {
	switch provider {
	case "gemini":
		// Every Gemini model takes a thinking budget, so all four map to
		// something real regardless of which model is selected.
	case "ollama":
		// Everything ollama's validator accepts. Which of these a given model
		// implements is decided by its chat template, not by ollama, and a
		// value the loaded model does not know fails the turn with the
		// template's own error — nothing on the wire reports a model's
		// supported set in advance, so the alternative to offering all of
		// them is putting the top of the scale out of reach.
		//
		// ollama 0.32.15 also renames some of them on the way in: measured
		// against a qwen3 build, none/minimal/low/medium arrive intact,
		// "high" arrives as "high", and xhigh/ultra/max all arrive as "max".
		// That template accepts only xhigh/medium/low, so its own default —
		// xhigh — is the one level no request can name.
		return []ReasoningEffort{
			ReasoningEffortNone, ReasoningEffortMinimal, ReasoningEffortLow, ReasoningEffortMedium,
			ReasoningEffortHigh, ReasoningEffortXHigh, ReasoningEffortUltra, ReasoningEffortMax,
		}
	case "openai", "openai-responses", "copilot", "huggingface", "lmstudio", "omniroute":
		if strings.Contains(model, "-chat") {
			return nil
		}
	default:
		return nil
	}
	return []ReasoningEffort{ReasoningEffortNone, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh}
}

// ServiceTier selects OpenAI's processing tier for a request. Flex trades
// higher latency for lower cost. It is OpenAI's own field: sent only for the
// "openai" provider and for "omniroute", which proxies straight through to
// OpenAI; every other OpenAI-compatible provider (copilot, lmstudio,
// huggingface) drops it. Leave it empty for the account default.
type ServiceTier string

const ServiceTierFlex ServiceTier = "flex"

// ToolChoice constrains whether the model may answer with plain content or
// must call one of the offered tools. Empty is the provider default ("auto":
// the model picks). Required forces a call to some offered tool, not to a
// particular one — narrow the Tools list to narrow the choice. It only means
// anything alongside a non-empty Tools list.
type ToolChoice string

const ToolChoiceRequired ToolChoice = "required"

type Request struct {
	Model           string          `json:"model"`
	Messages        []Message       `json:"messages"`
	Temperature     float64         `json:"temperature,omitempty"`
	MaxTokens       int             `json:"max_tokens,omitempty"`
	Tools           []Tool          `json:"tools,omitempty"`
	ToolChoice      ToolChoice      `json:"tool_choice,omitempty"`
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
	// SetTimeout changes how long a request may wait for response headers,
	// for calls made after it returns. See Session.SetTimeout for why this
	// exists separately from Config.Timeout: a provider built at startup
	// outlives the settings that follow it, and /timeout has to reach it.
	SetTimeout(d time.Duration)
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
	// ServiceTier is forwarded to the session for providers that pass it on
	// to OpenAI — see ServiceTier's own doc comment for which those are.
	ServiceTier ServiceTier `yaml:"service_tier"`
	// ReasoningEffort is the effort the session starts at, as last chosen for
	// this provider. Empty sends no reasoning field at all, which is the
	// model's own default.
	ReasoningEffort ReasoningEffort `yaml:"reasoning_effort"`
	// ContextLimit overrides the context window assumed for this provider's
	// models, in tokens. Zero leaves it to whatever catalogue the caller
	// consults — it is here for gateways and local servers, whose served
	// window is a deployment choice no catalogue can know.
	ContextLimit int `yaml:"context"`
	// Temp is the sampling temperature a session built from this config
	// starts at. Zero means unset, which leaves Session at its own default
	// (1.0) — a real override has to be a positive number, since 0 is itself
	// a meaningful (fully greedy) temperature some callers do want, just not
	// reachable as a config default here.
	Temp float64 `yaml:"temp"`
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
		p = newOpenAICompatProvider(cfg, defaultHFBaseURL, false)
	case "openai":
		// service_tier is OpenAI's own field: only this backend may send it.
		p = newOpenAICompatProvider(cfg, defaultOpenAIBaseURL, true)
	case "openai-responses":
		// Same models and the same key, over /v1/responses. Worth choosing
		// when a model refuses some combination on chat/completions — notably
		// function tools together with a reasoning effort. See responses.go.
		p = newResponsesProvider(cfg, defaultOpenAIBaseURL, true)
	case "gemini":
		p = newGeminiProvider(cfg)
	case "omniroute":
		// Proxies straight through to OpenAI, so its own service_tier is
		// meaningful and forwarded the same as the "openai" case above.
		p = newOpenAICompatProvider(cfg, defaultLMStudioBaseURL, true)
	case "lmstudio", "ollama":
		// Local OpenAI-compatible gateway: same transport, no API key, the
		// base_url from the config decides where requests go.
		p = newOpenAICompatProvider(cfg, defaultLMStudioBaseURL, false)
	default:
		return nil, fmt.Errorf("llm: unknown provider %q", cfg.Provider)
	}
	return &Client{provider: p}, nil
}

// SetTimeout changes how long the underlying provider waits for response
// headers, for requests made after it returns.
func (c *Client) SetTimeout(d time.Duration) { c.provider.SetTimeout(d) }

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
