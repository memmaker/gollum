package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/peterh/liner"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go-llm/llm"
	rec_files "go-llm/rec-files"
)

func main() {
	model := flag.String("model", "", "model name (overrides provider default)")
	interactive := flag.Bool("i", false, "interactive mode")
	flag.Parse()

	ctx := context.Background()

	// Ensure config exists under ~/.config/gollum/config.rec
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: cannot determine home directory:", err)
		os.Exit(1)
	}
	cfgDir := filepath.Join(home, ".config", "gollum")
	cfgPath := filepath.Join(cfgDir, "config.rec")

	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if err := os.MkdirAll(cfgDir, 0700); err != nil {
			fmt.Fprintln(os.Stderr, "error creating config dir:", err)
			os.Exit(1)
		}
		f, err := os.Create(cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error creating config file:", err)
			os.Exit(1)
		}
		// default config
		recs := []rec_files.Record{
			{
				{Name: "provider", Value: "openai"},
				{Name: "model", Value: "gpt-5-mini"},
			},
		}
		if err := rec_files.Write(f, recs); err != nil {
			fmt.Fprintln(os.Stderr, "error writing config file:", err)
			f.Close()
			os.Exit(1)
		}
		f.Close()
	}

	// Read config
	cfgFile, err := os.Open(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error opening config file:", err)
		os.Exit(1)
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

	var client *llm.Client
	if apiKey == "" {
		// If the config specifies a provider, require its API key in env
		var hint string
		switch provider {
		case "openai":
			hint = "OPENAI_API_KEY or OPENAI_API_TOKEN"
		case "huggingface":
			hint = "HUGGINGFACE_API_KEY or HF_API_KEY"
		case "copilot":
			hint = "GITHUB_TOKEN or COPILOT_TOKEN"
		case "gemini":
			hint = "GEMINI_API_KEY or GOOGLE_API_KEY"
		default:
			hint = "OPENAI_API_KEY or OPENAI_API_TOKEN"
		}
		fmt.Fprintln(os.Stderr, "error: no API key found for provider", provider)
		fmt.Fprintln(os.Stderr, "Set the provider API key in the environment (e.g.", hint+")")
		os.Exit(1)
	}

	client, err = llm.New(llm.Config{Provider: provider, APIKey: apiKey, Model: cfgModel})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error creating client from config:", err)
		os.Exit(1)
	}

	// read system prompt from ~/.config/gollum/system.md if present
	systemPath := filepath.Join(home, ".config", "gollum", "system.md")
	var systemPrompt string
	if b, err := os.ReadFile(systemPath); err == nil {
		systemPrompt = string(b)
	}

	if *interactive {
		runInteractive(ctx, client, *model, systemPrompt)
		return
	}

	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error reading stdin:", err)
		os.Exit(1)
	}
	if len(input) == 0 {
		fmt.Fprintln(os.Stderr, "error: empty input")
		os.Exit(1)
	}

	// build request, injecting system prompt if present
	msgs := make([]llm.Message, 0, 2)
	if systemPrompt != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: systemPrompt})
	}
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: string(input)})

	req := llm.Request{
		Model:    *model,
		Messages: msgs,
	}

	ch, err := client.Stream(ctx, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	for msg := range ch {
		fmt.Print(msg.Content)
	}
}

func runInteractive(ctx context.Context, client *llm.Client, model string, systemPrompt string) {
	sess := llm.NewSession(client, systemPrompt)
	sess.Model = model
	// Use a line editor (supports arrow keys, Ctrl-A/Ctrl-E, history)
	l := liner.NewLiner()
	defer l.Close()
	l.SetCtrlCAborts(true)

	// load history from ~/.config/gollum/history if present
	home, _ := os.UserHomeDir()
	histPath := filepath.Join(home, ".config", "gollum", "history")
	if f, err := os.Open(histPath); err == nil {
		l.ReadHistory(f)
		f.Close()
	}

	defer func() {
		if f, err := os.Create(histPath); err == nil {
			l.WriteHistory(f)
			f.Close()
		}
	}()

	for {
		line, err := l.Prompt("> ")
		if err == liner.ErrPromptAborted || err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "readline error: %v\n", err)
			break
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "/exit" {
			break
		}
		if trimmed == "" {
			// ignore empty input
			continue
		}
		l.AppendHistory(trimmed)

		// We want both streaming output and full tool support.
		// Strategy: start streaming to show partial content, and concurrently run SendWithTools
		// to get the final structured response and tool outputs. After SendWithTools returns
		// we print any tool messages and any assistant content not already printed by the stream.

		// Stream with tool support (single logical flow). This will stream assistant output,
		// execute tool calls when they appear, send tool results back to the model, and
		// continue streaming until the conversation turn finishes.
		ch, err := sess.StreamWithTools(ctx, line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
			continue
		}
		for msg := range ch {
			fmt.Print(msg.Content)
		}
		fmt.Println()
	}
	// liner handles input errors internally; nothing to do here
}
