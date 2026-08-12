package llm

import (
	"context"
)

// Default endpoints of the OpenAI-compatible backends. They differ in the URL
// and — for OpenAI itself — in whether `service_tier` may be sent; everything
// else about them is the same wire protocol, so they share one provider.
const (
	defaultOpenAIBaseURL   = "https://api.openai.com/v1"
	defaultLMStudioBaseURL = "http://localhost:1234/v1"
	defaultHFBaseURL       = "https://router.huggingface.co/v1"
)

type openAICompatProvider struct {
	transport *openaiTransport
	model     string
	baseURL   string
}

// newOpenAICompatProvider builds a provider for any backend speaking OpenAI's
// chat-completions API. defaultURL is used when the config names none;
// allowServiceTier is OpenAI-only — see openaiTransport.
func newOpenAICompatProvider(cfg Config, defaultURL string, allowServiceTier bool) *openAICompatProvider {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultURL
	}
	t := newOpenAITransport(cfg.APIKey, cfg.timeout())
	t.allowServiceTier = allowServiceTier
	return &openAICompatProvider{
		transport: t,
		model:     cfg.Model,
		baseURL:   baseURL,
	}
}

func (p *openAICompatProvider) url() string {
	return p.baseURL + "/chat/completions"
}

func (p *openAICompatProvider) Chat(ctx context.Context, req Request) (Response, error) {
	if req.Model == "" {
		req.Model = p.model
	}
	return p.transport.chat(ctx, p.url(), req)
}

func (p *openAICompatProvider) Stream(ctx context.Context, req Request) (<-chan Message, error) {
	if req.Model == "" {
		req.Model = p.model
	}
	return p.transport.stream(ctx, p.url(), req)
}
