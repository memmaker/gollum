package llm

import (
	"context"
)

const defaultHFBaseURL = "https://router.huggingface.co/v1"

type huggingFaceProvider struct {
	transport *openaiTransport
	model     string
	baseURL   string
}

func newHuggingFaceProvider(cfg Config) *huggingFaceProvider {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultHFBaseURL
	}
	return &huggingFaceProvider{
		transport: newOpenAITransport(cfg.APIKey),
		model:     cfg.Model,
		baseURL:   baseURL,
	}
}

func (p *huggingFaceProvider) url() string {
	return p.baseURL + "/chat/completions"
}

func (p *huggingFaceProvider) Chat(ctx context.Context, req Request) (Response, error) {
	if req.Model == "" {
		req.Model = p.model
	}
	return p.transport.chat(ctx, p.url(), req)
}

func (p *huggingFaceProvider) Stream(ctx context.Context, req Request) (<-chan Message, error) {
	if req.Model == "" {
		req.Model = p.model
	}
	return p.transport.stream(ctx, p.url(), req)
}
