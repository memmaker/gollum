package llm

import (
    "fmt"
    "os"
    "path/filepath"

    rec_files "gollum/rec-files"
)

// InitClientFromHome reads ~/.config/gollum/config.rec and system.md and
// returns a configured Client, the configured model (may be empty), and the
// system prompt (may be empty). If no API key is found for the configured
// provider, an error is returned.
func InitClientFromHome() (*Client, string, string, error) {
    home, err := os.UserHomeDir()
    if err != nil {
        return nil, "", "", fmt.Errorf("cannot determine home directory: %w", err)
    }
    cfgDir := filepath.Join(home, ".config", "gollum")
    cfgPath := filepath.Join(cfgDir, "config.rec")

    // ensure config exists
    if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
        if err := os.MkdirAll(cfgDir, 0700); err != nil {
            return nil, "", "", fmt.Errorf("error creating config dir: %w", err)
        }
        f, err := os.Create(cfgPath)
        if err != nil {
            return nil, "", "", fmt.Errorf("error creating config file: %w", err)
        }
        // default config
        recs := []rec_files.Record{
            {
                {Name: "provider", Value: "openai"},
                {Name: "model", Value: "gpt-5-mini"},
            },
        }
        if err := rec_files.Write(f, recs); err != nil {
            f.Close()
            return nil, "", "", fmt.Errorf("error writing config: %w", err)
        }
        f.Close()
    }

    // Read config
    cfgFile, err := os.Open(cfgPath)
    if err != nil {
        return nil, "", "", fmt.Errorf("error opening config file: %w", err)
    }
    recs := rec_files.Read(cfgFile)
    cfgFile.Close()

    provider := "openai"
    cfgModel := ""
    if len(recs) > 0 {
        m := recs[0].ToMap()
        if v, ok := m["provider"]; ok && v != "" {
            provider = v
        }
        if v, ok := m["model"]; ok && v != "" {
            cfgModel = v
        }
    }

    // Determine API key from env according to provider
    var apiKey string
    switch provider {
    case "openai":
        apiKey = os.Getenv("OPENAI_API_KEY")
        if apiKey == "" {
            apiKey = os.Getenv("OPENAI_API_TOKEN")
        }
    case "huggingface":
        apiKey = os.Getenv("HUGGINGFACE_API_KEY")
        if apiKey == "" {
            apiKey = os.Getenv("HF_API_KEY")
        }
    case "copilot":
        apiKey = os.Getenv("GITHUB_TOKEN")
        if apiKey == "" {
            apiKey = os.Getenv("COPILOT_TOKEN")
        }
    case "gemini":
        apiKey = os.Getenv("GEMINI_API_KEY")
        if apiKey == "" {
            apiKey = os.Getenv("GOOGLE_API_KEY")
        }
    default:
        apiKey = os.Getenv("OPENAI_API_KEY")
        if apiKey == "" {
            apiKey = os.Getenv("OPENAI_API_TOKEN")
        }
    }

    if apiKey == "" {
        return nil, "", "", fmt.Errorf("no API key found for provider %s; set the provider API key in the environment", provider)
    }

    client, err := New(Config{Provider: provider, APIKey: apiKey, Model: cfgModel})
    if err != nil {
        return nil, "", "", err
    }

    // read system prompt from ~/.config/gollum/system.md if present
    systemPath := filepath.Join(home, ".config", "gollum", "system.md")
    var systemPrompt string
    if b, err := os.ReadFile(systemPath); err == nil {
        systemPrompt = string(b)
    }

    return client, cfgModel, systemPrompt, nil
}
