package llm

import (
    "context"
    "fmt"
)

// Call performs a single LLM call with built-in tool support and returns the
// assistant's final content. If cfg.Provider or cfg.APIKey are empty, NewFromEnv
// will be used to create a client.
func Call(ctx context.Context, cfg Config, systemPrompt, userPrompt string) (string, error) {
    var client *Client
    var err error
    if cfg.Provider == "" && cfg.APIKey == "" {
        client, err = NewFromEnv()
        if err != nil {
            return "", fmt.Errorf("llm: unable to create client from env: %w", err)
        }
    } else {
        client, err = New(cfg)
        if err != nil {
            return "", fmt.Errorf("llm: unable to create client: %w", err)
        }
    }

    sess := NewSession(client, systemPrompt)
    sess.Model = cfg.Model
    resp, err := sess.SendWithTools(ctx, userPrompt, nil, nil)
    if err != nil {
        return "", err
    }
    return resp.Message.Content, nil
}

// StreamWithTools is a convenience wrapper that creates a session and streams
// assistant content while supporting tool calls. It returns the message stream
// channel from the session.
func StreamWithTools(ctx context.Context, cfg Config, systemPrompt, userPrompt string) (<-chan Message, error) {
    var client *Client
    var err error
    if cfg.Provider == "" && cfg.APIKey == "" {
        client, err = NewFromEnv()
        if err != nil {
            return nil, fmt.Errorf("llm: unable to create client from env: %w", err)
        }
    } else {
        client, err = New(cfg)
        if err != nil {
            return nil, fmt.Errorf("llm: unable to create client: %w", err)
        }
    }

    sess := NewSession(client, systemPrompt)
    sess.Model = cfg.Model
    return sess.StreamWithTools(ctx, userPrompt)
}
