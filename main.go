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
)

func main() {
	model := flag.String("model", "", "model name (overrides provider default)")
	interactive := flag.Bool("i", false, "interactive mode")
	flag.Parse()

	ctx := context.Background()

    client, cfgModel, systemPrompt, err := llm.InitClientFromHome()
    if err != nil {
        fmt.Fprintln(os.Stderr, "error initializing client:", err)
        os.Exit(1)
    }

    // If no model was provided on the command line, prefer the configured model
    if *model == "" {
        *model = cfgModel
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
