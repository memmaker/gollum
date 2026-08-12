package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// modelLister is implemented by providers that expose a model discovery
// endpoint.
type modelLister interface {
	ListModels(ctx context.Context) ([]string, error)
}

// ListModels asks the provider for the models it offers. It returns an error
// for providers without a discovery endpoint.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	l, ok := c.provider.(modelLister)
	if !ok {
		return nil, fmt.Errorf("llm: provider does not support model discovery")
	}
	return l.ListModels(ctx)
}

// listOpenAIModels queries an OpenAI-compatible /models endpoint.
func listOpenAIModels(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list models: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("list models: decode: %w", err)
	}
	names := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		names = append(names, m.ID)
	}
	sort.Strings(names)
	return names, nil
}

func (p *openAICompatProvider) ListModels(ctx context.Context) ([]string, error) {
	return listOpenAIModels(ctx, p.baseURL, p.transport.apiKey)
}

func (p *copilotProvider) ListModels(ctx context.Context) ([]string, error) {
	token, err := p.getToken(ctx)
	if err != nil {
		return nil, err
	}
	return listOpenAIModels(ctx, defaultCopilotBaseURL, token)
}

func (p *geminiProvider) ListModels(ctx context.Context) ([]string, error) {
	url := fmt.Sprintf("%s/models?key=%s", p.baseURL, p.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list models: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("list models: decode: %w", err)
	}
	names := make([]string, 0, len(payload.Models))
	for _, m := range payload.Models {
		names = append(names, strings.TrimPrefix(m.Name, "models/"))
	}
	sort.Strings(names)
	return names, nil
}
