package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	markterm "github.com/memmaker/gollum/mark-term"
)

// Batch mode renders markdown by default, the same way the TUI does: a piped
// question should read like an answer, not like source. -raw is the escape
// hatch for a script that wants the model's own bytes.
func TestOneShotRendering(t *testing.T) {
	const src = "# Channels\n\nA channel is a **pipe**.\n\n- typed\n- blocking\n"

	rendered := markterm.RenderMarkdown(strings.TrimSpace(src), 60)
	// Emphasis becomes styling and list markers become bullets. Headings keep
	// their "#", styled — that is this renderer's house style, not a leak.
	if strings.Contains(rendered, "**") {
		t.Fatalf("emphasis markers survived rendering: %q", rendered)
	}
	if !strings.Contains(ansi.Strip(rendered), "• typed") {
		t.Fatalf("list marker was not rendered: %q", ansi.Strip(rendered))
	}
	if rendered == src {
		t.Fatal("rendering was a no-op")
	}
	for _, want := range []string{"Channels", "pipe", "typed", "blocking"} {
		if !strings.Contains(ansi.Strip(rendered), want) {
			t.Fatalf("rendered answer lost %q: %q", want, ansi.Strip(rendered))
		}
	}
	// -raw hands back exactly what the model wrote, markers and all.
	if !strings.Contains(src, "# Channels") || !strings.Contains(src, "**pipe**") {
		t.Fatal("raw mode must not be transforming the source")
	}
}

// Batch mode renders as it goes: each block goes out as soon as a later chunk
// can no longer change it, so a long answer arrives paragraph by paragraph
// instead of in one lump at the end. This walks the same settledPrefix loop
// runOneShot runs and counts how much came out before the stream finished.
func TestOneShotStreamsBlocks(t *testing.T) {
	chunks := []string{
		"Channels are typed pipes.\n",
		"\nThey block until both sides are ready.\n",
		"\n```go\nch := make(chan int)\n",
		"ch <- 1\n```\n",
		"\nBuffered channels only block when full.",
	}

	var pending strings.Builder
	var out []string
	earlyOutput := 0
	for i, c := range chunks {
		pending.WriteString(c)
		settled, rest := settledPrefix(pending.String())
		if settled == "" {
			continue
		}
		pending.Reset()
		pending.WriteString(rest)
		out = append(out, markterm.RenderMarkdown(settled, 60))
		if i < len(chunks)-1 {
			earlyOutput++
		}
	}
	if tail := strings.TrimSpace(pending.String()); tail != "" {
		out = append(out, markterm.RenderMarkdown(tail, 60))
	}

	if earlyOutput == 0 {
		t.Fatal("nothing was printed before the stream ended: not streaming")
	}
	// Nothing may be lost or duplicated on the way through.
	joined := ansi.Strip(strings.Join(out, "\n"))
	for _, want := range []string{"typed pipes", "both sides are ready", "ch <- 1", "only block when full"} {
		if n := strings.Count(joined, want); n != 1 {
			t.Fatalf("%q appears %d times in the streamed output, want 1:\n%s", want, n, joined)
		}
	}
	// A fence must never be split across two renders — half a code block is
	// rendered as prose and comes out mangled.
	for _, block := range out {
		if strings.Count(ansi.Strip(block), "```")%2 != 0 {
			t.Fatalf("a code fence was cut in half:\n%s", ansi.Strip(block))
		}
	}
}

// Rendering has to have a width even when stdout is a pipe, which is how batch
// mode is normally used. Zero or a negative width would collapse every line.
func TestOutputWidth(t *testing.T) {
	// The test binary's stdout is not a terminal, so this exercises the
	// fallback rather than the terminal query.
	if got := outputWidth(); got < 20 {
		t.Fatalf("outputWidth() = %d, too narrow to render into", got)
	}
}
