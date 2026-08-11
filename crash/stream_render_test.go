package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/x/ansi"

	"github.com/memmaker/gollum/llm"
)

// A finished paragraph is rendered while the next one is still arriving, and
// the answer that ends up in the transcript is the same text either way.
func TestStreamingSettlesFinishedBlocks(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New()}
	m.layout(100, 20)

	for _, part := range []string{"# Title\n", "\nSome **bold** text.\n", "\nStill wri"} {
		m.produced = true
		m.chunk(llm.Message{Content: part})
	}
	// The heading settled into the transcript; the unfinished tail did not.
	if got := ansi.Strip(m.transcript.String()); !strings.Contains(got, "# Title") {
		t.Fatalf("heading not settled: %q", got)
	}
	if strings.Contains(m.transcript.String(), "Still wri") {
		t.Fatal("unfinished tail was flushed")
	}
	if got := m.answer.String(); !strings.Contains(got, "Still wri") {
		t.Fatalf("tail = %q", got)
	}

	m.finishTurn()
	out := ansi.Strip(m.transcript.String())
	for _, want := range []string{"# Title", "bold", "Still wri"} {
		if !strings.Contains(out, want) {
			t.Fatalf("transcript lost %q:\n%s", want, out)
		}
	}
}
