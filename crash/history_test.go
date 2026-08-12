package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbles/v2/help"
	tea "charm.land/bubbletea/v2"

	"github.com/memmaker/gollum/llm"
)

func historyTUI(t *testing.T, past ...string) *tui {
	t.Helper()
	m := &tui{sess: llm.NewSession(nil, ""), oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New(), history: past}
	m.histAt = len(m.history)
	m.layout(100, 30)
	return m
}

// Up recalls the last prompt, keeps walking back, and down walks forward again
// until the half-typed draft comes back.
func TestHistoryWalk(t *testing.T) {
	m := historyTUI(t, "first", "second")
	m.setValue("half typed")

	m.recall(-1)
	if got := m.value(); got != "second" {
		t.Fatalf("one up gave %q, want the newest entry", got)
	}
	m.recall(-1)
	if got := m.value(); got != "first" {
		t.Fatalf("two up gave %q, want the oldest entry", got)
	}
	m.recall(-1) // past the oldest: stays put
	if got := m.value(); got != "first" {
		t.Fatalf("up past the end gave %q", got)
	}
	m.recall(1)
	m.recall(1)
	if got := m.value(); got != "half typed" {
		t.Fatalf("walking back down gave %q, want the draft restored", got)
	}
	m.recall(1) // past the draft: nothing newer
	if got := m.value(); got != "half typed" {
		t.Fatalf("down past the draft gave %q", got)
	}
}

// Sending a prompt appends it to the file and starts the next walk from the
// newest entry, not from wherever the last one stopped.
func TestHistoryRemembers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".config", "gollum"), 0o700)

	m := historyTUI(t, "old")
	m.recall(-1) // walk back, so histAt is not at the end
	m.remember("a multi\nline prompt")
	m.endRecall()

	if got := m.histAt; got != len(m.history) {
		t.Fatalf("histAt is %d of %d: the next up would resume mid-walk", got, len(m.history))
	}
	// A repeat of the last prompt is not stored twice.
	m.remember("a multi\nline prompt")
	if n := len(m.history); n != 2 {
		t.Fatalf("history holds %d entries, want 2", n)
	}

	b, err := os.ReadFile(filepath.Join(home, ".config", "gollum", "history"))
	if err != nil {
		t.Fatal(err)
	}
	// The newline is escaped, so one prompt stays one line.
	if got := string(b); got != `a multi\nline prompt`+"\n" {
		t.Fatalf("file holds %q", got)
	}
	if got := loadHistory(); len(got) != 1 || got[0] != "a multi\nline prompt" {
		t.Fatalf("reload gave %q, want the newline back", got)
	}
	if strings.Contains(string(b), "old") {
		t.Fatal("entries loaded from the file were written back a second time")
	}
}

// The key routing: the one-line prompt has no lines for the cursor to walk,
// so up and down always recall there. The multiline box is the opposite —
// they only ever move the cursor, so a long draft is never lost to a stray
// arrow key.
func TestHistoryKeys(t *testing.T) {
	m := historyTUI(t, "earlier")
	m.focus = paneInput

	up := tea.KeyPressMsg{Code: tea.KeyUp}
	m.Update(up)
	if got := m.value(); got != "earlier" {
		t.Fatalf("up on an empty box gave %q, want the last prompt", got)
	}

	// Open the multiline box with a two-line draft: up and down move the
	// cursor and never touch history.
	m = historyTUI(t, "earlier")
	m.focus = paneInput
	m.toggleMultiline()
	m.setValue("one\ntwo")
	if m.multiLine.Line() != 1 {
		t.Fatalf("cursor is on line %d, want the second", m.multiLine.Line())
	}
	m.Update(up)
	if got := m.value(); got != "one\ntwo" {
		t.Fatalf("up in the multiline box replaced the text with %q", got)
	}
	if m.multiLine.Line() != 0 {
		t.Fatal("up in the multiline box did not move the cursor")
	}
	m.Update(up) // still the first line: nowhere left to go, still no recall
	if got := m.value(); got != "one\ntwo" {
		t.Fatalf("up at the top of the multiline box gave %q, want the draft untouched", got)
	}
}
