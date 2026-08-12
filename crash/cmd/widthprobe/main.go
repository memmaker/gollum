// Command widthprobe asks the terminal how it really measures emoji.
//
// There are two answers and no way to infer which one a terminal uses: legacy
// wcwidth reads ⚠️ (U+26A0 + VS16) as one cell, Unicode 9 grapheme clustering
// reads it as two. DEC mode 2027 is meant to settle it, but a terminal can
// render Unicode 9 widths without implementing the mode — iTerm2's "Unicode 9
// widths" preference does exactly that — and then every layout that trusted
// the mode is off by a column per emoji.
//
// So this measures instead of asking: print a glyph, request the cursor
// position, and see how far it actually moved.
//
// Run it in the terminal you are debugging:
//
//	go run ./cmd/widthprobe
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

// samples are glyphs where the two rules disagree, plus a control that pins
// down whether the measurement itself works.
var samples = []struct{ name, glyph string }{
	{"A (control)", "A"},
	{"U+26A0 warning, bare", "⚠"},
	{"U+26A0 + VS16", "⚠️"},
	{"rocket U+1F680", "\U0001F680"},
	{"thumb + skin tone", "\U0001F44D\U0001F3FD"},
	{"family ZWJ sequence", "\U0001F468‍\U0001F469‍\U0001F467"},
}

func main() {
	in, out := os.Stdin, os.Stdout
	if !term.IsTerminal(in.Fd()) {
		fmt.Fprintln(os.Stderr, "widthprobe must run in a terminal, not a pipe")
		os.Exit(1)
	}
	state, err := term.MakeRaw(in.Fd())
	if err != nil {
		fmt.Fprintln(os.Stderr, "raw mode:", err)
		os.Exit(1)
	}
	restore := func() { _ = term.Restore(in.Fd(), state) }
	defer restore()

	fmt.Fprint(out, ansi.SetAltScreenSaveCursorMode+ansi.HideCursor)
	mode := queryMode(in, out)
	widths := make([]int, len(samples))
	for i, s := range samples {
		widths[i] = measure(in, out, s.glyph)
	}
	fmt.Fprint(out, ansi.ResetAltScreenSaveCursorMode+ansi.ShowCursor)
	restore()

	report(mode, widths)
}

func report(mode string, widths []int) {
	fmt.Println("TERM         =", os.Getenv("TERM"))
	fmt.Println("TERM_PROGRAM =", os.Getenv("TERM_PROGRAM"), os.Getenv("TERM_PROGRAM_VERSION"))
	fmt.Println("DEC mode 2027:", mode)
	fmt.Println()
	fmt.Printf("%-24s %8s %8s %8s   %s\n", "glyph", "actual", "wcwidth", "grapheme", "agrees with")

	seen := map[string]bool{}
	for i, s := range samples {
		wc := ansi.WcWidth.StringWidth(s.glyph)
		gr := ansi.GraphemeWidth.StringWidth(s.glyph)
		verdict := ""
		switch {
		case widths[i] < 0:
			verdict = "no reply"
		case wc == gr:
			verdict = "both (no information)"
		case widths[i] == wc:
			verdict = "wcwidth"
			seen["wcwidth"] = true
		case widths[i] == gr:
			verdict = "grapheme"
			seen["grapheme"] = true
		default:
			verdict = "neither"
			seen["neither"] = true
		}
		fmt.Printf("%-24s %8d %8d %8d   %s\n", s.name, widths[i], wc, gr, verdict)
	}

	fmt.Println()
	switch {
	case seen["neither"]:
		fmt.Println("VERDICT: matches neither rule — paste the whole table")
	case seen["wcwidth"] && seen["grapheme"]:
		fmt.Println("VERDICT: inconsistent between glyphs — paste the whole table")
	case seen["grapheme"]:
		fmt.Println("VERDICT: GraphemeWidth")
	case seen["wcwidth"]:
		fmt.Println("VERDICT: WcWidth")
	default:
		fmt.Println("VERDICT: nothing measured — the terminal answered no position request")
	}
}

// queryMode reports what the terminal says about DEC mode 2027.
func queryMode(in, out *os.File) string {
	fmt.Fprint(out, ansi.RequestModeUnicodeCore)
	reply := read(in, 300*time.Millisecond)
	if reply == "" {
		return "no reply (mode not recognized)"
	}
	// DECRPM: CSI ? 2027 ; Ps $ y
	i := strings.Index(reply, "2027;")
	if i < 0 {
		return fmt.Sprintf("unparsed reply %q", reply)
	}
	rest := reply[i+len("2027;"):]
	j := strings.IndexByte(rest, '$')
	if j <= 0 {
		return fmt.Sprintf("unparsed reply %q", reply)
	}
	switch rest[:j] {
	case "0":
		return "0 — not recognized"
	case "1":
		return "1 — set (terminal uses grapheme widths)"
	case "2":
		return "2 — reset, but supported (terminal can do grapheme widths)"
	case "3":
		return "3 — permanently set (grapheme widths)"
	case "4":
		return "4 — permanently reset (wcwidth only)"
	}
	return fmt.Sprintf("unparsed reply %q", reply)
}

// measure prints glyph at the start of an empty line and reports how many
// columns the cursor advanced. Returns -1 when the terminal does not answer.
func measure(in, out *os.File, glyph string) int {
	fmt.Fprint(out, "\x1b[1;1H"+ansi.EraseEntireLine+glyph+ansi.RequestCursorPositionReport)
	reply := read(in, 300*time.Millisecond)
	// CPR: CSI row ; col R, and the column is 1-based.
	i := strings.LastIndexByte(reply, '[')
	j := strings.LastIndexByte(reply, 'R')
	if i < 0 || j < i {
		return -1
	}
	parts := strings.Split(reply[i+1:j], ";")
	if len(parts) != 2 {
		return -1
	}
	col, err := strconv.Atoi(parts[1])
	if err != nil {
		return -1
	}
	return col - 1
}

func read(in *os.File, wait time.Duration) string {
	_ = in.SetReadDeadline(time.Now().Add(wait))
	defer func() { _ = in.SetReadDeadline(time.Time{}) }()
	var b strings.Builder
	buf := make([]byte, 256)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			b.Write(buf[:n])
		}
		if err != nil || n == 0 {
			return b.String()
		}
		if strings.ContainsAny(b.String(), "Ry") {
			return b.String()
		}
	}
}
