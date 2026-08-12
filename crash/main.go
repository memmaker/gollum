package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"

	markterm "github.com/memmaker/gollum/mark-term"
	"github.com/memmaker/gollum/llm"
)

func main() {
	model := flag.String("model", "", "model name (overrides provider default)")
	batch := flag.Bool("b", false, "batch mode: answer the prompt on stdin and exit")
	raw := flag.Bool("raw", false, "print the answer as the model wrote it, markdown source and all")
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
		runOneShot(ctx, cfgs[0], *model, *raw)
		return
	}
	runInteractive(ctx, cfgs, *model, *raw)
}

// runOneShot answers whatever is on stdin and exits.
//
// The answer goes through the markdown renderer, the same one the TUI uses, so
// a piped question reads like an answer rather than like source. That costs the
// streaming: markdown cannot be rendered a chunk at a time, since a fence or a
// table is only known to be one once it closes. -raw keeps the old behaviour
// and streams the model's own text through untouched, which is what a script
// consuming the output wants.
func runOneShot(ctx context.Context, cfg llm.Config, model string, raw bool) {
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
	width := outputWidth()
	var pending strings.Builder
	for msg := range ch {
		if msg.Err != nil {
			fmt.Fprintf(os.Stderr, "\nerror: %v\n", msg.Err)
			failed = true
			continue
		}
		if raw {
			fmt.Print(msg.Content)
			continue
		}
		pending.WriteString(msg.Content)
		// Print each block as soon as later chunks can no longer change it.
		// Only the block still being written is held back, so the answer
		// arrives paragraph by paragraph rather than all at the end.
		settled, rest := settledPrefix(pending.String())
		if settled == "" {
			continue
		}
		pending.Reset()
		pending.WriteString(rest)
		fmt.Println(markterm.RenderMarkdown(settled, width) + "\n")
	}
	// The last block never settles — nothing follows it to prove it is done —
	// and whatever arrived before an error is still worth printing.
	if !raw {
		if tail := strings.TrimSpace(pending.String()); tail != "" {
			fmt.Println(markterm.RenderMarkdown(tail, width))
		}
	}
	if failed {
		os.Exit(1)
	}
}

// outputWidth is how wide the rendered answer may run. Redirected into a file
// or a pipe there is no width to ask for, so it falls back to the conventional
// 80 columns rather than to whatever the last terminal happened to be.
func outputWidth() int {
	if w, _, err := term.GetSize(os.Stdout.Fd()); err == nil && w > 0 {
		return w
	}
	return 80
}

func die(args ...any) {
	fmt.Fprintln(os.Stderr, args...)
	os.Exit(1)
}
