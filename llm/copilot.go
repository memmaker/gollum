package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const defaultCopilotBaseURL = "https://api.githubcopilot.com"

type copilotProvider struct {
	apiKey    string
	model     string
	client    *http.Client
	mu        sync.Mutex
	cached    string
	expiresAt int64
}

type copilotTokenResp struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

func newCopilotProvider(cfg Config) *copilotProvider {
	return &copilotProvider{
		apiKey: cfg.APIKey,
		model:  cfg.Model,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (p *copilotProvider) getToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	if p.cached != "" && time.Now().Unix() < p.expiresAt-120 {
		token := p.cached
		p.mu.Unlock()
		return token, nil
	}
	p.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, defaultCopilotBaseURL+"/token", bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("copilot token exchange: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("copilot token exchange: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tr copilotTokenResp
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("copilot token exchange: decode: %w", err)
	}
	if tr.Token == "" {
		return "", fmt.Errorf("copilot token exchange: empty token")
	}

	p.mu.Lock()
	p.cached = tr.Token
	p.expiresAt = tr.ExpiresAt
	p.mu.Unlock()

	return tr.Token, nil
}

func (p *copilotProvider) Chat(ctx context.Context, req Request) (Response, error) {
	token, err := p.getToken(ctx)
	if err != nil {
		return Response{}, err
	}
	if req.Model == "" {
		req.Model = p.model
	}
	trans := newOpenAITransport(token)
	return trans.chat(ctx, defaultCopilotBaseURL+"/chat/completions", req)
}

func (p *copilotProvider) Stream(ctx context.Context, req Request) (<-chan Message, error) {
	token, err := p.getToken(ctx)
	if err != nil {
		return nil, err
	}
	if req.Model == "" {
		req.Model = p.model
	}
	trans := newOpenAITransport(token)
	return trans.stream(ctx, defaultCopilotBaseURL+"/chat/completions", req)
}
