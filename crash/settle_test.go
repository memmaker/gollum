package main

import (
	"strings"
	"testing"
)

// Streaming must only hand over text that later chunks cannot change. Cutting
// inside a fence or between the halves of one list corrupts the render.
func TestSettledPrefix(t *testing.T) {
	tests := []struct {
		name, in, settled, rest string
	}{
		{"nothing complete", "a partial senten", "", "a partial senten"},
		{"one finished paragraph", "done.\n\nstill writ", "done.", "still writ"},
		{"blank inside open fence never cuts", "text\n\n```go\nx := 1\n\ny := 2", "text", "```go\nx := 1\n\ny := 2"},
		{"closed fence settles", "```go\nx := 1\n```\n\nnext par", "```go\nx := 1\n```", "next par"},
		{"loose list stays whole", "- one\n\n- two\n\nreal end\n\ntail", "- one\n\n- two\n\nreal end", "tail"},
		{"table stays whole", "| a |\n\n| b |\n\nafter\n\nt", "| a |\n\n| b |\n\nafter", "t"},
	}
	for _, tc := range tests {
		settled, rest := settledPrefix(tc.in)
		if settled != tc.settled || rest != tc.rest {
			t.Errorf("%s:\n settled = %q, want %q\n rest = %q, want %q", tc.name, settled, tc.settled, rest, tc.rest)
		}
		// Whatever the split, no text is lost.
		if joined := strings.TrimSpace(settled + "\n\n" + rest); joined != strings.TrimSpace(tc.in) {
			t.Errorf("%s: split lost text: %q", tc.name, joined)
		}
	}
}
