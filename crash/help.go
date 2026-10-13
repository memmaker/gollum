package main

import "charm.land/bubbles/v2/key"

// helpText is what /help shows. Markdown, so the pager renders it as a table
// and ctrl+r still gives the source.
const helpText = `Type any of these in the prompt instead of a question.

| command | what it does |
|:--------|:-------------|
| ` + "`/help`" + ` | this page |
| ` + "`/new`" + ` | start a fresh conversation: the model forgets everything said so far |
| ` + "`/model`" + ` | pick another provider and model, keeping the conversation |
| ` + "`/compress`" + ` | replace the conversation with a summary of it, to win back context |
| ` + "`/context`" + ` | report the model's context window; ` + "`/context 64000`" + ` sets it |
| ` + "`/timeout`" + ` | report the idle limit; ` + "`/timeout 5m`" + ` sets it, ` + "`/timeout 0`" + ` turns it off |
| ` + "`/debug`" + ` | toggle full tool arguments and results in the transcript |
| ` + "`/exit`" + ` | quit — the conversation is saved to ` + "`" + sessionFile + "`" + ` and restored next time |
`

// hotkeyRow pairs a keymap binding with where it applies. Pulling the key and
// description out of keys (tui.go) instead of retyping them here means a new
// or rebound key shows up automatically instead of only in the bottom help
// bar — that gap is how ctrl+s went undocumented.
type hotkeyRow struct {
	where string
	b     key.Binding
}

var hotkeyRows = []hotkeyRow{
	{"anywhere", keys.menu},
	{"anywhere", keys.session},
	{"anywhere", keys.next},
	{"anywhere", keys.quit},
	{"while a reply is streaming", keys.interrupt},
	{"one-line prompt", keys.send},
	{"one-line prompt", keys.history},
	{"multiline box (ctrl+space)", keys.altEnter},
	{"multiline box (ctrl+space)", keys.newlineHint},
	{"input pane", keys.multiline},
	{"output/side pane", keys.up},
	{"output/side pane", keys.down},
	{"output/side pane", keys.top},
	{"output/side pane", keys.bottom},
	{"output pane, once a tool ran", keys.blockUp},
	{"output pane, block selected", keys.fold},
	{"a /help-style page", keys.render},
	{"menus and pages", keys.cancel},
}

// hotkeysText renders hotkeyRows as the markdown table appended after
// helpText.
func hotkeysText() string {
	s := "\n| key | where | what it does |\n|:----|:------|:-------------|\n"
	for _, r := range hotkeyRows {
		h := r.b.Help()
		s += "| `" + h.Key + "` | " + r.where + " | " + h.Desc + " |\n"
	}
	return s
}
