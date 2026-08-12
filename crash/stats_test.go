package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/memmaker/gollum/llm"
)

func TestTokenStats(t *testing.T) {
	var s tokenStats
	s.addCall("openai/gpt-5", "hello there", 0, true, llm.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15, ReasoningTokens: 2})
	s.addCall("openai/gpt-5", strings.Repeat("x", 100), 2, false, llm.Usage{InputTokens: 20, OutputTokens: 1, TotalTokens: 21})
	s.addCall("gemini/pro", "what\n  is  up", 1, true, llm.Usage{InputTokens: 3, OutputTokens: 4, TotalTokens: 7, ReasoningTokens: 1})

	if got := s.total(); got != (llm.Usage{InputTokens: 33, OutputTokens: 10, TotalTokens: 43, ReasoningTokens: 3}) {
		t.Fatalf("total = %+v", got)
	}
	if s.calls != 3 || len(s.byModel["openai/gpt-5"]) != 2 {
		t.Fatalf("calls=%d per-model=%d", s.calls, len(s.byModel["openai/gpt-5"]))
	}
	if got := s.perModel("gemini/pro"); got.TotalTokens != 7 {
		t.Fatalf("perModel = %+v", got)
	}
	if got := s.byModel["gemini/pro"][0].Context; got != "what is up" {
		t.Fatalf("context = %q", got)
	}
	if got := s.byModel["openai/gpt-5"][1].Context; ansi.StringWidth(got) > contextPreviewLen {
		t.Fatalf("context not trimmed: %q", got)
	}
	// gemini sorts before openai, and every model gets its own markdown
	// heading with a table row per call.
	out := ansi.Strip(s.String())
	if !strings.HasPrefix(out, "## gemini/pro\n") {
		t.Fatalf("String = %q", out)
	}
	if got := strings.Count(out, "| context |"); got != 2 {
		t.Fatalf("want one table header per model, got %d:\n%s", got, out)
	}
	// The tool-call count and the prose flag are their own columns now.
	if !strings.Contains(out, "| 2 | no |") {
		t.Fatalf("String missing the 2-tools/no-prose row:\n%s", out)
	}
	if !strings.Contains(out, "| 0 | yes |") {
		t.Fatalf("String missing the no-tools/prose row:\n%s", out)
	}
}

// A prompt containing a pipe must not split its own table row.
func TestStatsEscapesPipesInContext(t *testing.T) {
	var s tokenStats
	s.addCall("openai/gpt-5", "grep foo | wc -l", 0, true, llm.Usage{TotalTokens: 1})
	if !strings.Contains(s.String(), `grep foo \| wc -l`) {
		t.Fatalf("pipe not escaped:\n%s", s.String())
	}
}
