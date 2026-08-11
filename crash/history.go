package main

import (
	"os"
	"path/filepath"
	"strings"
)

// The prompt history lives where the readline version of this program kept it,
// so a user who came from that one still has theirs: one entry per line in
// ~/.config/gollum/history, oldest first.
//
// A prompt can span lines — alt+enter inserts one — and the file cannot, so
// newlines are written as `\n` and read back. Entries are appended as they are
// sent rather than dumped on exit: a crash then costs nothing.
func historyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "gollum", "history"), nil
}

// loadHistory reads the saved prompts. A missing file is an empty history, not
// an error worth a message.
func loadHistory() []string {
	path, err := historyPath()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line != "" {
			out = append(out, strings.ReplaceAll(line, `\n`, "\n"))
		}
	}
	return out
}

// remember adds one sent prompt to the history and to the file. A prompt sent
// twice in a row is kept once: pressing up should reach the thing before it,
// not the same line again.
func (m *tui) remember(prompt string) {
	if prompt == "" {
		return
	}
	if n := len(m.history); n > 0 && m.history[n-1] == prompt {
		return
	}
	m.history = append(m.history, prompt)

	path, err := historyPath()
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(strings.ReplaceAll(prompt, "\n", `\n`) + "\n")
}

// recall steps through the history by delta and puts the entry in the input
// box. Stepping past the newest entry brings back whatever was being typed
// when the walk started, which is the only copy of it there is.
func (m *tui) recall(delta int) {
	if len(m.history) == 0 {
		return
	}
	if m.histAt == len(m.history) {
		if delta > 0 {
			return // already at the draft: there is nothing newer
		}
		m.draft = m.input.Value()
	}
	at := m.histAt + delta
	if at < 0 {
		at = 0
	}
	if at > len(m.history) {
		at = len(m.history)
	}
	if at == m.histAt {
		return
	}
	m.histAt = at
	if at == len(m.history) {
		m.input.SetValue(m.draft)
	} else {
		m.input.SetValue(m.history[at])
	}
	m.input.CursorEnd()
}

// endRecall leaves the history walk, so the next up starts from the newest
// entry again rather than from wherever the last walk stopped.
func (m *tui) endRecall() {
	m.histAt = len(m.history)
	m.draft = ""
}
