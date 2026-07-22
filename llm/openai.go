package llm

import (
	"context"
)

const defaultOpenAIBaseURL = "https://api.openai.com/v1"

type openAIProvider struct {
	transport *openaiTransport
	model     string
	baseURL   string
}

func newOpenAIProvider(cfg Config) *openAIProvider {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultOpenAIBaseURL
	}
	return &openAIProvider{
		transport: newOpenAITransport(cfg.APIKey),
		model:     cfg.Model,
		baseURL:   baseURL,
	}
}

func (p *openAIProvider) url() string {
	return p.baseURL + "/chat/completions"
}

func (p *openAIProvider) Chat(ctx context.Context, req Request) (Response, error) {
	if req.Model == "" {
		req.Model = p.model
	}
	return p.transport.chat(ctx, p.url(), req)
}

func (p *openAIProvider) Stream(ctx context.Context, req Request) (<-chan Message, error) {
	if req.Model == "" {
		req.Model = p.model
	}
	return p.transport.stream(ctx, p.url(), req)
}
