package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"

	markterm "github.com/memmaker/gollum/mark-term"
	"github.com/memmaker/gollum/llm"
)

func main() {
	model := flag.String("model", "", "model name (overrides provider default)")
	batch := flag.Bool("b", false, "batch mode: answer the prompt on stdin and exit")
	prompt := flag.String("p", "", "non-interactive mode: answer this prompt, running any tool calls to completion, printing everything to stdout")
	raw := flag.Bool("raw", false, "print the answer as the model wrote it, markdown source and all")
	timeout := flag.Duration("timeout", llm.DefaultTimeout, "give up when a backend sends nothing for this long")
	metrics := flag.Bool("metrics", false, "with -p, print a JSON line of tool-call/token/timing stats to stderr when the run ends")
	temp := flag.Float64("temp", -1, "with -p, sampling temperature — lower is more deterministic, worth trying around 0.2-0.3 for a small local model's tool-call reliability. Negative (the default) defers to the config file's `temp:`, or 1.0 if that is unset too")
	reasoning := flag.String("reasoning", "", "with -p, override the configured reasoning effort (none/low/medium/high, plus minimal/xhigh/ultra/max on ollama) — empty keeps the config file's value")
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
	if *prompt != "" {
		if *reasoning != "" {
			cfgs[0].ReasoningEffort = llm.ReasoningEffort(*reasoning)
		}
		effTemp := *temp
		if effTemp < 0 {
			effTemp = cfgs[0].Temp
		}
		if effTemp <= 0 {
			effTemp = 1.0
		}
		runPrompt(ctx, cfgs, *model, *raw, *prompt, *metrics, effTemp)
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

	// The saved effort applies here too: batch mode reads the same record the
	// TUI writes, and honouring its model but not its reasoning level would
	// answer the same question differently in the two modes.
	ch, err := client.Stream(ctx, llm.Request{
		Model:           model,
		Messages:        msgs,
		ReasoningEffort: validEffort(cfg.Provider, model, cfg.ReasoningEffort),
	})
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

// runPrompt answers one prompt non-interactively, running the same tool-call
// chain the TUI would, and prints everything that would have landed in the
// output pane to stdout: reasoning, tool calls, tool results, and the answer
// itself. Returns once the chain ends (no more tool calls).
func runPrompt(ctx context.Context, cfgs []llm.Config, model string, raw bool, prompt string, metrics bool, temp float64) {
	settings := LoadSettings()
	if cfgs[0].Timeout == llm.DefaultTimeout {
		cfgs[0].Timeout = settings.Timeout
	}
	client, err := llm.New(cfgs[0])
	if err != nil {
		die("error initializing client:", err)
	}
	sess := llm.NewSession(client, SystemPrompt())
	sess.Model = model
	sess.Temp = temp
	sess.SendRegisteredTools = settings.SendTools
	sess.Stateless = !settings.Session
	sess.AutoCompact = settings.AutoCompact
	sess.Timeout = cfgs[0].Timeout
	sess.ServiceTier = cfgs[0].ServiceTier
	sess.ReasoningEffort = validEffort(cfgs[0].Provider, model, cfgs[0].ReasoningEffort)
	// The interactive TUI resolves this at startup (tui.go's refreshContextLimit)
	// so auto-compaction has a window to compact against; this path skipped it
	// entirely, which silently disabled AutoCompact no matter the setting — a
	// long tool chain just ran until the backend rejected an oversized request.
	pricingCtx, cancelPricing := context.WithTimeout(ctx, 5*time.Second)
	prices := loadPricing(pricingCtx)
	cancelPricing()
	sess.ContextLimit, _ = resolveContextSize(cfgs[0].Provider+"/"+model, cfgs, prices)

	toolConfigs, _ := LoadToolConfigs()
	registerTools(sess, toolConfigs)
	mcpConfigs, _ := LoadMCPConfigs()
	mcpClients, _ := startMCPs(sess, mcpConfigs, toolConfigs)
	defer func() {
		for _, c := range mcpClients {
			c.close()
		}
	}()

	start := time.Now()
	ch, err := sess.StreamWithTools(ctx, prompt)
	if err != nil {
		die("error:", err)
	}

	width := outputWidth()
	var tt tui // zero value: renderToolCall/renderToolResult only touch m.out when full is false
	var reasoning, answer strings.Builder
	failed := false
	toolCalls, toolErrors, compactions := 0, 0, 0
	for msg := range ch {
		if msg.Usage != nil {
			continue
		}
		if msg.Compacted {
			compactions++
			if tail := strings.TrimSpace(answer.String()); tail != "" && !raw {
				fmt.Println(markterm.RenderMarkdown(tail, width) + "\n")
			}
			answer.Reset()
			if msg.Err != nil {
				fmt.Println("compaction failed:", msg.Err)
			} else {
				fmt.Println("compacted the conversation to fit the context window")
			}
			continue
		}
		if msg.Err != nil {
			fmt.Fprintf(os.Stderr, "\nerror: %v\n", msg.Err)
			failed = true
			continue
		}
		if msg.Reasoning != "" {
			reasoning.WriteString(msg.Reasoning)
		}
		switch {
		case len(msg.ToolCalls) > 0:
			if reasoning.Len() > 0 {
				fmt.Println(dim.Render("◆ thinking"))
				fmt.Println(dim.Render(markterm.Wrap(reasoning.String(), width)))
				reasoning.Reset()
			}
			if tail := strings.TrimSpace(answer.String()); tail != "" && !raw {
				fmt.Println(markterm.RenderMarkdown(tail, width) + "\n")
			}
			answer.Reset()
			toolCalls += len(msg.ToolCalls)
			for _, tc := range msg.ToolCalls {
				fmt.Println(tt.renderToolCall(tc, true, false))
			}
		case msg.Role == llm.RoleTool:
			// Every tool executor in this harness reports its own failure —
			// bad args, a missing file, an unknown tool name — by returning a
			// result that starts with "error:" (see session.go's StreamWithTools
			// and tools.go's execX functions), so that prefix is the one place
			// a failed call can be told from a successful one after the fact.
			if strings.HasPrefix(msg.Content, "error:") {
				toolErrors++
			}
			fmt.Println(tt.renderToolResult(msg, true))
		case msg.Content != "":
			if reasoning.Len() > 0 {
				fmt.Println(dim.Render("◆ thinking"))
				fmt.Println(dim.Render(markterm.Wrap(reasoning.String(), width)))
				reasoning.Reset()
			}
			if raw {
				fmt.Print(msg.Content)
				continue
			}
			answer.WriteString(msg.Content)
			settled, rest := settledPrefix(answer.String())
			if settled == "" {
				continue
			}
			answer.Reset()
			answer.WriteString(rest)
			fmt.Println(markterm.RenderMarkdown(settled, width) + "\n")
		}
	}
	if !raw {
		if tail := strings.TrimSpace(answer.String()); tail != "" {
			fmt.Println(markterm.RenderMarkdown(tail, width))
		}
	}
	if metrics {
		u := sess.Usage()
		enc := json.NewEncoder(os.Stderr)
		enc.Encode(map[string]any{
			"elapsed_seconds":  time.Since(start).Seconds(),
			"tool_calls":       toolCalls,
			"tool_call_errors": toolErrors,
			"compactions":      compactions,
			"input_tokens":     u.InputTokens,
			"output_tokens":    u.OutputTokens,
			"total_tokens":     u.TotalTokens,
			"reasoning_tokens": u.ReasoningTokens,
			"failed":           failed,
		})
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
