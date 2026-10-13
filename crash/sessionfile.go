package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	markterm "github.com/memmaker/gollum/mark-term"
	rec_files "github.com/memmaker/rec-files"

	"github.com/memmaker/gollum/llm"
)

// sessionFile is the running record of one conversation: in the working
// directory, so a project keeps its own thread and starting crash somewhere
// else starts clean. Picked up again by the next crash started here.
//
// It is written as it happens rather than dumped on the way out — every
// message the session appends, and every harness event that replaces the
// history — and only ever appended to. A crash mid-turn then costs nothing,
// and the file at any moment is the conversation at that moment.
//
// The format is recfile, the same one the config is written in: one record
// per event, `role:` records for messages and `event:` records for what the
// harness did to them.
const sessionFile = ".crash_session"

// sessionLog appends to sessionFile. The session calls it from whichever
// goroutine made the change — the streaming one, mostly — so it serializes
// itself.
type sessionLog struct {
	mu  sync.Mutex
	f   *os.File
	err error // the first write that failed, reported once at exit
}

// openSessionLog opens sessionFile for appending. A log that cannot be opened
// is nil, which every method tolerates: losing the record of a session is not
// a reason to refuse to hold one.
func openSessionLog() *sessionLog {
	f, err := os.OpenFile(sessionFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return &sessionLog{err: err}
	}
	return &sessionLog{f: f}
}

// write appends messages as records. With replace set, the history was thrown
// away and rebuilt — compacted, or cleared by /new — so a reset event goes in
// front of the messages that replaced it, and reading the file back stops at
// that point rather than replaying what is gone.
func (l *sessionLog) write(msgs []llm.Message, replace bool) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	var b strings.Builder
	if replace {
		writeRecord(&b, rec_files.Record{{Name: "event", Value: "reset"}})
	}
	for _, m := range msgs {
		if m.Role == llm.RoleSystem {
			// The prompt belongs to the config, not to the conversation:
			// pinning one here would outlive every edit to it.
			continue
		}
		writeRecord(&b, messageRecord(m))
	}
	if b.Len() == 0 {
		return
	}
	if _, err := l.f.WriteString(b.String()); err != nil && l.err == nil {
		l.err = err
	}
}

// note records something the harness did that is not a message — a model
// switch, a compaction that failed. Nothing reads these back; they are there
// for whoever reads the file.
func (l *sessionLog) note(what string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	var b strings.Builder
	writeRecord(&b, rec_files.Record{{Name: "event", Value: what}})
	if _, err := l.f.WriteString(b.String()); err != nil && l.err == nil {
		l.err = err
	}
}

// close flushes nothing — every write already went to the file — and reports
// the first write that failed, if one did.
func (l *sessionLog) close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	return l.err
}

// messageRecord turns one message into the record it is written as. Only the
// fields that go back to a provider are kept: reasoning, usage and the rest
// are per-run detail that a restored conversation neither sends nor shows.
func messageRecord(m llm.Message) rec_files.Record {
	rec := rec_files.Record{{Name: "role", Value: m.Role}}
	if m.Name != "" {
		rec = append(rec, rec_files.Field{Name: "name", Value: m.Name})
	}
	if m.ToolCallID != "" {
		rec = append(rec, rec_files.Field{Name: "tool_call_id", Value: m.ToolCallID})
	}
	if len(m.ToolCalls) > 0 {
		// JSON on one line: a tool call is a structure, and recfile has no
		// shape for one. It is the only field here that is not plain text.
		if b, err := json.Marshal(m.ToolCalls); err == nil {
			rec = append(rec, rec_files.Field{Name: "tool_calls", Value: string(b)})
		}
	}
	// Content last: it is the long one, and a multi-line value is written as
	// continuation lines, so anything after it would be harder to spot.
	rec = append(rec, rec_files.Field{Name: "content", Value: m.Content})
	return rec
}

// writeRecord appends one record to b. rec_files.Write is not usable here: it
// prefixes every call with a `%rec:` header, and a reader hitting a second one
// throws away every record it had read before it.
//
// ponytail: values keep no trailing whitespace on a line — the reader trims
// it — which is invisible in prose and in code, and would only show up in a
// tool result that carried it.
func writeRecord(b *strings.Builder, rec rec_files.Record) {
	for _, f := range rec {
		b.WriteString(f.Name + ": " + f.EscapedValue() + "\n")
	}
	b.WriteString("\n")
}

// loadSession reads the log back into the conversation it describes. A reset
// event drops everything read so far: it marks the point where a compaction
// or a /new replaced the history, and what follows it is that replacement.
//
// No file, or one that cannot be read as a transcript, is a fresh start rather
// than a failure to launch: the file is a convenience, and refusing to open on
// a corrupt one would strand the user in the very directory they work in.
//
// ponytail: the reader scans line by line with a 64KB line cap, so a single
// logged line longer than that ends the replay early. Chunking long values
// across physical lines is the fix, if a tool result ever hits it.
func loadSession() []llm.Message {
	f, err := os.Open(sessionFile)
	if err != nil {
		return nil
	}
	defer f.Close()

	var msgs []llm.Message
	for _, rec := range rec_files.Read(f) {
		m := rec.ToMap("\n")
		if event := m["event"]; event != "" {
			if event == "reset" {
				msgs = nil
			}
			continue
		}
		if m["role"] == "" {
			continue
		}
		msg := llm.Message{
			Role:       m["role"],
			Content:    m["content"],
			Name:       m["name"],
			ToolCallID: m["tool_call_id"],
		}
		if raw := m["tool_calls"]; raw != "" {
			json.Unmarshal([]byte(raw), &msg.ToolCalls)
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

// replayRestoredHistory draws a conversation loaded from sessionFile into the
// transcript, in the same shapes submit and chunk would have produced it
// live — a restored session reads like one you scrolled back through, not
// like a note that it happened. Called once, from layout's first resize:
// rendering needs the pane width, which nothing knows before that.
func (m *tui) replayRestoredHistory() {
	if len(m.pendingRestore) == 0 {
		return
	}
	msgs := m.pendingRestore
	m.pendingRestore = nil
	m.transcript.WriteString(dim.Render(fmt.Sprintf("restored %d messages from %s", len(msgs), sessionFile)) + "\n\n")
	for _, msg := range msgs {
		switch msg.Role {
		case llm.RoleUser:
			m.transcript.WriteString(label.Render(markterm.Wrap("> "+msg.Content, max(m.out.Width(), 10))) + "\n\n")
		case llm.RoleAssistant:
			if strings.TrimSpace(msg.Content) != "" {
				body := msg.Content
				if !m.raw {
					body = markterm.RenderMarkdown(body, m.out.Width())
				}
				m.transcript.WriteString(body + "\n\n")
			}
			for _, tc := range msg.ToolCalls {
				m.addToolCall(tc)
			}
		case llm.RoleTool:
			m.addToolResult(msg)
		}
		m.endBlock()
	}
	m.follow = true
}
