package llm

import (
	"context"
)

const defaultLMStudioBaseURL = "http://localhost:1234/v1"

type lmstudioProvider struct {
	transport *openaiTransport
	model     string
	baseURL   string
}

func newLMStudioProvider(cfg Config) *lmstudioProvider {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultLMStudioBaseURL
	}
	return &lmstudioProvider{
		transport: newOpenAITransport(cfg.APIKey),
		model:     cfg.Model,
		baseURL:   baseURL,
	}
}

func (p *lmstudioProvider) url() string {
	return p.baseURL + "/chat/completions"
}

func (p *lmstudioProvider) Chat(ctx context.Context, req Request) (Response, error) {
	if req.Model == "" {
		req.Model = p.model
	}
	return p.transport.chat(ctx, p.url(), req)
}

func (p *lmstudioProvider) Stream(ctx context.Context, req Request) (<-chan Message, error) {
	if req.Model == "" {
		req.Model = p.model
	}
	return p.transport.stream(ctx, p.url(), req)
}
