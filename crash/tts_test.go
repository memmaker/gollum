package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSpeakable(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "Hello there.", "Hello there."},
		// A heading is a sentence: it gets the full stop the engine needs to
		// pause on, unless it already ends in punctuation of its own.
		{"headers", "## Result\n\nIt worked.", "Result. It worked."},
		{"heading keeps its own punctuation", "# Done!\n\nnext", "Done! next"},
		{"heading ending in a colon", "## Steps:\n\nfirst", "Steps: first"},
		{"heading with trailing marker", "### Result ###\n\nok", "Result. ok"},
		{"bullets", "- one\n- two\n1. three", "one two three"},
		{"emphasis", "This is **bold** and *italic* and `code`.", "This is bold and italic and code."},
		{"underscores", "The send_tools flag is _off_.", "The send tools flag is off."},
		{"link", "See [the docs](https://example.com) for more.", "See the docs for more."},
		{"image", "Look: ![a chart](chart.png) done.", "Look: done."},
		{"quote", "> quoted line", "quoted line"},
		{"rule", "before\n\n---\n\nafter", "before after"},
		{"fence dropped", "Here:\n\n```go\nfor i := range x {\n}\n```\n\nDone.", "Here: Done."},
		{"tilde fence dropped", "Here:\n\n~~~\nraw\n~~~\n\nDone.", "Here: Done."},
		{"unclosed fence", "Here:\n\n```go\nfor i := range x {", "Here:"},
		// A table read aloud loses the columns that gave the cells meaning.
		{"table dropped", "before\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\nafter", "before after"},
		{"only a table", "| a | b |\n|---|---|\n| 1 | 2 |", ""},
		{"only code", "```\nx := 1\n```", ""},
		{"empty", "   \n\n  ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := speakable(c.in); got != c.want {
				t.Errorf("speakable(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// A fake engine speaking the real protocol: the SERVING handshake, a log line
// the worker must ignore, then one TTS_TIME reply per request.
const fakeEngine = `#!/bin/sh
echo SERVING
while read -r line; do
  echo "loading voice, please wait"
  wav=$(mktemp "$WAVDIR/render.XXXXXX")
  printf '%s\n' "$line" >> "$WAVDIR/spoken.log"
  echo "TTS_TIME 0.1s $wav"
done
`

// The worker has to survive noise on the pipe, clean up every render, and let
// Close return — the failure mode of the last one is a hung test.
func TestTTSWorkerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	engine := filepath.Join(dir, "fake-engine")
	if err := os.WriteFile(engine, []byte(fakeEngine), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAVDIR", dir)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	failed := make(chan error, 1)
	tts, err := startTTS(engine, func(err error) { failed <- err })
	if err != nil {
		t.Fatalf("startTTS: %v", err)
	}
	tts.Speak("## Hello _there_")

	// The log proves the block reached the engine as stripped prose, past the
	// noise line the worker had to skip.
	log := filepath.Join(dir, "spoken.log")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(log); err == nil && strings.TrimSpace(string(b)) == "Hello there." {
			break
		} else if err == nil {
			t.Fatalf("engine received %q", b)
		}
		if time.Now().After(deadline) {
			t.Fatal("engine never received the text")
		}
		time.Sleep(20 * time.Millisecond)
	}

	done := make(chan struct{})
	go func() { tts.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return: the worker is still holding the pipe")
	}
	select {
	case err := <-failed:
		t.Fatalf("engine reported a failure: %v", err)
	default:
	}
	// Every render is deleted after playing, so the working directory does not
	// fill up over a long session.
	left, _ := filepath.Glob(filepath.Join(dir, "render.*"))
	if len(left) != 0 {
		t.Fatalf("renders left behind: %v", left)
	}
}
