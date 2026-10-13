package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
)

// Every command submit acts on has to be in helpText, or /help quietly goes
// stale the next time one is added. Read out of the source because that is
// where the list actually lives — a second list here would rot the same way.
func TestHelpCoversEveryCommand(t *testing.T) {
	src, err := os.ReadFile("tui.go")
	if err != nil {
		t.Fatal(err)
	}
	cmds := regexp.MustCompile(`"(/[a-z]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(cmds) < 5 {
		t.Fatalf("found only %d commands in tui.go — has submit moved?", len(cmds))
	}
	for _, c := range cmds {
		if !strings.Contains(helpText, "`"+c[1]+"`") {
			t.Errorf("%s is not in helpText", c[1])
		}
	}
}

// TestHotkeysCoverEveryBinding lists every field of keymap directly (same
// package, so the unexported fields are readable here). A binding missing
// from this list, or from hotkeysText, is how ctrl+s went undocumented —
// this fails loudly the next time a binding is added to keys but not here.
func TestHotkeysCoverEveryBinding(t *testing.T) {
	all := map[string]key.Binding{
		"send": keys.send, "altEnter": keys.altEnter, "newlineHint": keys.newlineHint,
		"multiline": keys.multiline, "oneLineHint": keys.oneLineHint, "next": keys.next,
		"up": keys.up, "down": keys.down, "top": keys.top, "bottom": keys.bottom,
		"quit": keys.quit, "pick": keys.pick, "cancel": keys.cancel, "menu": keys.menu,
		"history": keys.history, "session": keys.session, "render": keys.render,
		"interrupt": keys.interrupt, "blockUp": keys.blockUp, "blockDown": keys.blockDown,
		"fold": keys.fold,
	}
	// Bindings whose key is already documented via another row (a shared key,
	// different description for a different pane), so they don't need one of
	// their own.
	hintOnly := map[string]bool{"oneLineHint": true, "blockDown": true, "pick": true}

	text := hotkeysText()
	for name, b := range all {
		if hintOnly[name] {
			continue
		}
		if desc := b.Help().Desc; !strings.Contains(text, desc) {
			t.Errorf("keymap field %q (%q) is not documented in hotkeysText", name, desc)
		}
	}
}
