package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/memmaker/gollum/llm"
)

func main() {
	model := flag.String("model", "", "model name (overrides provider default)")
	batch := flag.Bool("b", false, "batch mode: answer the prompt on stdin and exit")
	raw := flag.Bool("raw", false, "stream raw text instead of rendering markdown")
	timeout := flag.Duration("timeout", llm.DefaultTimeout, "give up when a backend sends nothing for this long")
	flag.Parse()

	if err := LoadColors(); err != nil {
		die("error loading colors:", err)
	}
	cfgs, err := LoadConfigs()
	if err != nil {
		die("error initializing client:", err)
	}
	for i := range cfgs {
		cfgs[i].Timeout = *timeout
	}
	// If no model was provided on the command line, prefer the configured model
	if *model == "" {
		*model = cfgs[0].Model
	}

	ctx := context.Background()
	if *batch {
		runOneShot(ctx, cfgs[0], *model)
		return
	}
	runInteractive(ctx, cfgs, *model, *raw)
}

// runOneShot answers whatever is on stdin and exits.
func runOneShot(ctx context.Context, cfg llm.Config, model string) {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		die("error reading stdin:", err)
	}
	if len(input) == 0 {
		die("error: empty input")
	}
	client, err := llm.New(cfg)
	if err != nil {
		die("error initializing client:", err)
	}

	msgs := make([]llm.Message, 0, 2)
	if sp := SystemPrompt(); sp != "" {
		msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: sp})
	}
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: string(input)})

	// One-shot mode has no prompt to hand back to, so the whole call is bounded.
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	ch, err := client.Stream(ctx, llm.Request{Model: model, Messages: msgs})
	if err != nil {
		die("error:", err)
	}
	failed := false
	for msg := range ch {
		if msg.Err != nil {
			fmt.Fprintf(os.Stderr, "\nerror: %v\n", msg.Err)
			failed = true
			continue
		}
		fmt.Print(msg.Content)
	}
	if failed {
		os.Exit(1)
	}
}

func die(args ...any) {
	fmt.Fprintln(os.Stderr, args...)
	os.Exit(1)
}
