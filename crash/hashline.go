package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/OneOfOne/xxhash"
	"github.com/memmaker/gollum/llm"
)

// Hashline is a line-addressed edit format: every line a model reads comes
// with a short hash of its content, and every edit quotes that hash back. An
// edit whose hash no longer matches is refused rather than applied to whatever
// happens to sit at that line number now, which is what makes it safe to hand
// a model a file it read several turns ago.
//
// The operations are the hashline CLI's, so a model that knows that server
// needs to learn nothing here: SWAP, DEL, INS.PRE, INS.POST, INS.HEAD,
// INS.TAIL, their .BLK forms, and CUT/PUT registers, over single lines or
// N:hash..M ranges.
//
// What is not the CLI's is the strictness, which is the point of owning this.
// Every anchor carries a hash and is checked; the CLI takes `DEL 15` on
// trust. A line that looks like an operation but names an unknown one is an
// error here; the MCP server folds it into the previous operation's payload
// and writes it into the file as literal text, silently, whenever some other
// operation in the same patch succeeded. Payload lines are written verbatim,
// with no leading `+` to strip — and a patch that carries the CLI's `+`
// prefixes throughout is refused rather than written with them.

// lineHash is the anchor a patch quotes back to prove it is editing the line
// it actually read. Two bytes: cheap enough to repeat on every line of a file
// the model pays for by the token, and a 1-in-65536 chance that an edited line
// hashes the same as the one that was read and the edit lands on it anyway.
func lineHash(s string) string {
	return fmt.Sprintf("%04x", xxhash.ChecksumString32(s)&0xffff)
}

// splitLines cuts a file the way the format numbers it. A file ending in a
// newline yields a final empty line, and joining the result back with "\n"
// reproduces the original byte for byte — including whether it ended in a
// newline at all.
func splitLines(content string) []string { return strings.Split(content, "\n") }

// renderHashline is what the model reads: a header naming the file, then one
// numbered, hashed line per line of it.
func renderHashline(path string, lines []string) string {
	return renderHashlineRange(path, lines, 1, len(lines))
}

// renderHashlineRange prints lines first..last only, keeping the line numbers
// they have in the file and the header hash of the whole file: an excerpt
// still says which version of the file its anchors belong to.
func renderHashlineRange(path string, lines []string, first, last int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s#%s]\n", path, lineHash(strings.Join(lines, "\n")))
	for i := first; i <= last; i++ {
		fmt.Fprintf(&b, "%d:%s|%s\n", i, lineHash(lines[i-1]), lines[i-1])
	}
	return b.String()
}

// renderHashlineLines prints an arbitrary, non-contiguous set of lines — as
// outline does for the signatures it picked out — keeping their real line
// numbers so the anchors line up with what a full read or patch would show.
func renderHashlineLines(path string, lines []string, nums []int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s#%s]\n", path, lineHash(strings.Join(lines, "\n")))
	for _, n := range nums {
		fmt.Fprintf(&b, "%d:%s|%s\n", n, lineHash(lines[n-1]), lines[n-1])
	}
	return b.String()
}

// outlineSignature matches a line that looks like a top-level declaration:
// a Go func/type, a Python def/class, or a JS/TS function/class/arrow-const.
// ponytail: one heuristic regex, not a real parser — misses signatures that
// wrap onto a second line and any language it doesn't list. Good enough for
// scanning a file's shape; reach for a real parser (or tree-sitter) if a
// language's declarations routinely span lines or this list keeps growing.
var outlineSignature = regexp.MustCompile(
	`^\s*(` +
		`func\s|` + // Go
		`type\s+\w+\s+(struct|interface)\b|` + // Go
		`(async\s+)?def\s|class\s|` + // Python
		`(export\s+)?(default\s+)?(async\s+)?function\s|` + // JS/TS
		`(export\s+)?class\s|` + // JS/TS
		`(export\s+)?(const|let|var)\s+\w+\s*=\s*(async\s*)?\(.*=>` + // JS/TS arrow
		`)`)

// hlAnchor matches a LINE:HASH handed to a tool on its own, outside a patch.
var hlAnchor = regexp.MustCompile(`^[ \t]*(\d+):([0-9a-fA-F]+)[ \t]*:*[ \t]*$`)

// resolveAnchor turns a LINE:HASH into its line number, refusing it if that
// line has changed since it was read — the same check a patch operation makes.
func resolveAnchor(lines []string, s string) (int, error) {
	m := hlAnchor.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%q is not a LINE:HASH anchor like `12:a3f1`", s)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%q is not a line number", m[1])
	}
	if n > len(lines) {
		return 0, fmt.Errorf("%s: the file has only %d lines", s, len(lines))
	}
	if got := lineHash(lines[n-1]); got != strings.ToLower(m[2]) {
		return 0, fmt.Errorf("%s: line %d has changed since it was read — it now reads %d:%s|%s", s, n, n, got, lines[n-1])
	}
	return n, nil
}

// indentOf counts a line's leading whitespace, tabs and spaces alike.
func indentOf(s string) int { return len(s) - len(strings.TrimLeft(s, " \t")) }

// isBlockCloser spots the lines that end a block in the languages that indent
// one: a brace, bracket or paren, or a closing keyword.
func isBlockCloser(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	if strings.ContainsAny(t[:1], "}])") {
		return true
	}
	switch strings.Fields(t)[0] {
	case "end", "fi", "done", "esac":
		return true
	}
	return false
}

// findBlock guesses how far the block around line n reaches. A line whose
// next non-blank neighbour is indented deeper opens its own block; any other
// line sits inside one, and the block returned is the enclosing one.
//
// ponytail: indentation heuristic, no parser. It knows nothing of multi-line
// strings, here-docs or a language that puts the opening brace on its own
// line. Swap in a real parser per language if that ever matters.
func findBlock(lines []string, n int) (int, int) {
	blank := func(i int) bool { return strings.TrimSpace(lines[i-1]) == "" }

	start, opens := n, false
	for i := n + 1; i <= len(lines); i++ {
		if blank(i) {
			continue
		}
		opens = indentOf(lines[i-1]) > indentOf(lines[n-1])
		break
	}
	if !opens {
		for start > 1 && (blank(start) || indentOf(lines[start-1]) >= indentOf(lines[n-1])) {
			start--
		}
	}

	indent, end := indentOf(lines[start-1]), start
	for i := start + 1; i <= len(lines); i++ {
		if blank(i) {
			continue
		}
		if indentOf(lines[i-1]) <= indent {
			// A closer at the opening line's own indentation ends this block
			// rather than starting the next one.
			if isBlockCloser(lines[i-1]) {
				end = i
			}
			break
		}
		end = i
	}
	return start, end
}

// hlOp is one operation of a patch: what to do, which lines to do it at, the
// hashes those lines are expected to have, the register a CUT or PUT names,
// and the lines written in its place.
type hlOp struct {
	kind     string
	line     int
	hash     string
	end      int    // last line of a N:hash..M range, 0 for a single line
	endHash  string // the range's closing hash, empty when it was left off
	register string // CUT/PUT only: the @name, empty for the anonymous one
	payload  []string
}

// hlOps are the operations, and whether each one takes replacement lines.
// SWAP replaces lines, DEL removes them, INS.* put new lines somewhere
// without touching what is there, and the .BLK forms take the block around
// their anchor instead of the anchored line — the same block find_block
// prints. CUT lifts lines out into a register and PUT pastes them back.
var hlOps = map[string]bool{
	"SWAP": true, "DEL": false,
	"INS.PRE": true, "INS.POST": true, "INS.HEAD": true, "INS.TAIL": true,
	"SWAP.BLK": true, "DEL.BLK": false, "INS.BLK.PRE": true, "INS.BLK.POST": true, "INS.BLK": true,
	"CUT": false, "PUT": false,
}

var (
	// hlDirective matches an operation line: an upper-case op name, the
	// line:hash anchor it applies to, optionally a ..line:hash end of range
	// and a @register. Deliberately loose about the op name so that a
	// misspelled one is recognised as an attempted operation and rejected,
	// rather than being taken for content and written into the file. The
	// trailing colon and the `.=` range spelling are the CLI's, accepted so
	// that muscle memory for it lands somewhere useful.
	hlDirective = regexp.MustCompile(`^([A-Z][A-Z.]*)[ \t]+(\d+):([0-9a-fA-F]+)(?:(?:\.\.|\.=)(\d+)(?::([0-9a-fA-F]+))?)?(?:[ \t]+@([A-Za-z0-9_-]+))?[ \t]*:*[ \t]*$`)

	// hlPut matches PUT's own shape, which names where it pastes with a `<`.
	hlPut = regexp.MustCompile(`^PUT(?:[ \t]+@([A-Za-z0-9_-]+))?[ \t]*<[ \t]*(\d+):([0-9a-fA-F]+)[ \t]*:*[ \t]*$`)

	// hlAnchorless matches the operations that need no anchor: the two ends
	// of the file, and a PUT that pastes at the head of it.
	hlAnchorless = regexp.MustCompile(`^(INS\.HEAD|INS\.TAIL|PUT)(?:[ \t]+@([A-Za-z0-9_-]+))?[ \t]*:*[ \t]*$`)

	// hlOpWord catches a known operation written in some shape none of the
	// above accept, so that `DEL 15` — an anchor with no hash, which the CLI
	// would take on trust — is an error and not a line of content.
	hlOpWord = regexp.MustCompile(`^([A-Z][A-Z.]*)(?:[ \t]|$)`)

	// hlBareLine matches an anchor with a line number and nothing else worth a
	// hash: just digits, or digits with the colon that should have introduced
	// one, either end of a range or both — the shape a model lands on when it
	// drops the hash on every operation after the patch's first, and the shape
	// it lands on when it patches straight off a compiler or test failure,
	// which hands out line numbers and no hashes at all.
	hlBareLine = regexp.MustCompile(`^(\d+):?(?:(?:\.\.|\.=)(\d+):?)?$`)

	// hlHashOnly matches the opposite mistake: one or two hashes with no line
	// number in front of them, as when a model quotes the hash a read printed
	// but drops the "LINE:" it was attached to — SWAP 8e4f..73d7 instead of
	// SWAP 9:8e4f..121:73d7.
	hlHashOnly = regexp.MustCompile(`^[0-9a-fA-F]{2,8}(?:\.\.[0-9a-fA-F]{2,8})?:?$`)
)

// parseOpLine reads one line as an operation. The middle return says whether
// it was one at all; a line that is not belongs to the operation above it.
func parseOpLine(line string) (hlOp, bool, error) {
	if m := hlPut.FindStringSubmatch(line); m != nil {
		n, _ := strconv.Atoi(m[2])
		return hlOp{kind: "PUT", line: n, hash: strings.ToLower(m[3]), register: m[1]}, true, nil
	}
	if m := hlAnchorless.FindStringSubmatch(line); m != nil {
		return hlOp{kind: m[1], register: m[2]}, true, nil
	}
	if m := hlDirective.FindStringSubmatch(line); m != nil {
		if _, ok := hlOps[m[1]]; !ok {
			return hlOp{}, false, fmt.Errorf("unknown operation %q — use %s", m[1], hlOpNames())
		}
		n, err := strconv.Atoi(m[2])
		if err != nil || n < 1 {
			return hlOp{}, false, fmt.Errorf("%q is not a line number", m[2])
		}
		op := hlOp{kind: m[1], line: n, hash: strings.ToLower(m[3]), register: m[6]}
		if m[4] != "" {
			end, err := strconv.Atoi(m[4])
			if err != nil || end < 1 {
				return hlOp{}, false, fmt.Errorf("%q is not a line number", m[4])
			}
			op.end, op.endHash = end, strings.ToLower(m[5])
		}
		return op, true, nil
	}
	if m := hlOpWord.FindStringSubmatch(line); m != nil {
		if _, ok := hlOps[m[1]]; ok {
			rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), m[1]))
			if am := hlBareLine.FindStringSubmatch(rest); am != nil {
				want := am[1] + ":a3f1"
				if am[2] != "" {
					want += ".." + am[2] + ":9c2b"
				}
				return hlOp{}, false, fmt.Errorf("%s %s has no hash — every operation needs its own, not just the first one in a patch. A line number from a compiler, a test or a stack trace is not an anchor: read the file (read or find_block) and quote the hash it prints beside the line, like `%s %s`", m[1], rest, m[1], want)
			}
			if hlHashOnly.MatchString(rest) {
				firstHash := strings.TrimSuffix(strings.SplitN(rest, "..", 2)[0], ":")
				return hlOp{}, false, fmt.Errorf("%s %s is missing its line number — an anchor is LINE:HASH, not the hash alone, like `%s 12:%s`", m[1], rest, m[1], firstHash)
			}
			return hlOp{}, false, fmt.Errorf("%q is not a valid %s anchor — use LINE:HASH, like `%s 12:a3f1`", line, m[1], m[1])
		}
	}
	return hlOp{}, false, nil
}

// hlOpNames lists the operations for an error message, in a fixed order:
// hlOps is a map, and a name that moves around between two runs of the same
// mistake reads like the tool is guessing.
func hlOpNames() string {
	names := make([]string, 0, len(hlOps))
	for name := range hlOps {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// stripEnvelope removes the `*** Begin Patch` wrapper a patch may arrive in,
// along with the `[path#hash]` header line inside it and the closing marker.
// An `*** Abort` anywhere means the patch was withdrawn while it was being
// written, and nothing in it is applied.
func stripEnvelope(patch string) (string, error) {
	lines := splitLines(patch)
	for _, line := range lines {
		if strings.TrimSpace(line) == "*** Abort" {
			return "", fmt.Errorf("patch says *** Abort — nothing applied")
		}
	}
	first := 0
	for first < len(lines) && strings.TrimSpace(lines[first]) == "" {
		first++
	}
	if first == len(lines) || strings.TrimSpace(lines[first]) != "*** Begin Patch" {
		return patch, nil
	}
	first++
	if first < len(lines) && hlHeader.MatchString(lines[first]) {
		first++
	}
	last := len(lines)
	for last > first && strings.TrimSpace(lines[last-1]) == "" {
		last--
	}
	if last > first && strings.TrimSpace(lines[last-1]) == "*** End Patch" {
		last--
	}
	return strings.Join(lines[first:last], "\n"), nil
}

// hlHeader matches the `[path#hash]` line that opens a read, which a patch
// may repeat inside its envelope to say which version it was written against.
var hlHeader = regexp.MustCompile(`^\[.*#[0-9a-fA-F]+\][ \t]*$`)

// parseHashlinePatch reads a patch into its operations. Everything that is not
// an operation line belongs to the operation above it, so the first line of a
// patch has to be one.
func parseHashlinePatch(patch string) ([]hlOp, error) {
	// One trailing newline is the line ending of the last payload line, not an
	// empty line after it. A patch that means to end a file with a blank line
	// says so with two.
	body, err := stripEnvelope(strings.TrimSuffix(patch, "\n"))
	if err != nil {
		return nil, err
	}
	var ops []hlOp
	for i, line := range splitLines(body) {
		op, isOp, err := parseOpLine(line)
		if err != nil {
			return nil, fmt.Errorf("patch line %d: %w", i+1, err)
		}
		if !isOp {
			if len(ops) == 0 {
				if strings.TrimSpace(line) == "" {
					continue // leading blank lines, before anything has been said
				}
				return nil, fmt.Errorf("patch line %d: expected an operation like `SWAP 12:a3f1`, got %q", i+1, line)
			}
			ops[len(ops)-1].payload = append(ops[len(ops)-1].payload, line)
			continue
		}
		ops = append(ops, op)
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("patch contains no operations")
	}
	for i, op := range ops {
		if takes := hlOps[op.kind]; !takes && len(op.payload) > 0 {
			return nil, fmt.Errorf("%s %d:%s takes no replacement lines — use SWAP to put something in their place", op.kind, op.line, op.hash)
		}
		payload, err := stripPlus(op.payload)
		if err != nil {
			return nil, fmt.Errorf("%s %d:%s: %w", op.kind, op.line, op.hash, err)
		}
		ops[i].payload = payload
	}
	return ops, nil
}

// stripPlus takes the `+` off an operation's replacement lines when every one
// of them carries it, which is how the hashline CLI marks a patch body. A body
// written without them is taken verbatim, as the CLI takes that too; a line
// that really has to start with a `+` is written `++`. Empty lines carry no
// marker either way and are left alone.
func stripPlus(payload []string) ([]string, error) {
	marked, content := 0, 0
	for _, line := range payload {
		if line == "" {
			continue
		}
		content++
		if strings.HasPrefix(line, "+") {
			marked++
		}
	}
	if marked == 0 {
		return payload, nil
	}
	if marked != content {
		return nil, fmt.Errorf("some replacement lines start with `+` and some do not — mark all of them or none")
	}
	out := make([]string, len(payload))
	for i, line := range payload {
		out[i] = strings.TrimPrefix(line, "+")
	}
	return out, nil
}

// edit collects everything one line of the original file has happening to it.
type edit struct {
	pre, post []string
	swap      []string
	swapped   bool
	deleted   bool
}

// resolve turns one operation's anchors into the range of lines it covers,
// refusing any anchor whose line has changed since it was read.
func (op hlOp) resolve(lines []string) (int, int, error) {
	switch op.kind {
	case "INS.HEAD":
		return 1, 1, nil
	case "INS.TAIL":
		return len(lines), len(lines), nil
	case "PUT":
		if op.line == 0 { // a bare PUT pastes at the head of the file
			return 1, 1, nil
		}
	}
	if op.line > len(lines) {
		return 0, 0, fmt.Errorf("%s %d:%s: the file has only %d lines", op.kind, op.line, op.hash, len(lines))
	}
	if got := lineHash(lines[op.line-1]); got != op.hash {
		return 0, 0, fmt.Errorf("%s %d:%s: line %d has changed since it was read — it now reads %d:%s|%s", op.kind, op.line, op.hash, op.line, op.line, got, lines[op.line-1])
	}
	from, to := op.line, op.line
	if op.end != 0 {
		if op.end > len(lines) {
			return 0, 0, fmt.Errorf("%s %d:%s..%d: the file has only %d lines", op.kind, op.line, op.hash, op.end, len(lines))
		}
		if op.end < op.line {
			return 0, 0, fmt.Errorf("%s %d:%s..%d: the range ends before it starts", op.kind, op.line, op.hash, op.end)
		}
		if op.endHash != "" {
			if got := lineHash(lines[op.end-1]); got != op.endHash {
				return 0, 0, fmt.Errorf("%s %d:%s..%d:%s: line %d has changed since it was read — it now reads %d:%s|%s", op.kind, op.line, op.hash, op.end, op.endHash, op.end, op.end, got, lines[op.end-1])
			}
		}
		to = op.end
	}
	if strings.Contains(op.kind, ".BLK") {
		// The block the anchor sits in, whatever range was written: a .BLK
		// operation is asking for exactly what find_block would have printed.
		from, to = findBlock(lines, op.line)
	}
	return from, to, nil
}

// applyHashline resolves every anchor against the file as it was read, then
// writes the result in one pass. Nothing is resolved against a half-edited
// buffer, so the operations of a patch neither have to be in order nor have to
// account for how much the ones before them grew or shrank the file.
func applyHashline(lines []string, ops []hlOp) ([]string, int, int, error) {
	edits := map[int]*edit{}
	at := func(n int) *edit {
		if edits[n] == nil {
			edits[n] = &edit{}
		}
		return edits[n]
	}
	minLine, maxLine := len(lines)+1, 0
	// Two operations claiming the same line have no defined order and no
	// obvious meaning: refuse rather than pick one.
	cut := func(from, to int) error {
		for n := from; n <= to; n++ {
			e := at(n)
			if e.swapped || e.deleted {
				return fmt.Errorf("line %d is edited twice in one patch", n)
			}
			e.deleted = true
		}
		return nil
	}

	registers := map[string][]string{}
	// CUT first, so that a PUT pastes what its CUT captured whichever order
	// the two were written in — like every other anchor here, they both refer
	// to the file as it was read.
	for _, cutting := range []bool{true, false} {
		for _, op := range ops {
			if (op.kind == "CUT") != cutting {
				continue
			}
			from, to, err := op.resolve(lines)
			if err != nil {
				return nil, 0, 0, err
			}
			if from < minLine {
				minLine = from
			}
			if to > maxLine {
				maxLine = to
			}
			switch op.kind {
			case "CUT":
				if _, dup := registers[op.register]; dup {
					return nil, 0, 0, fmt.Errorf("two CUTs into the same register — name them apart with @somename")
				}
				registers[op.register] = append([]string(nil), lines[from-1:to]...)
				if err := cut(from, to); err != nil {
					return nil, 0, 0, err
				}
			case "PUT":
				held, ok := registers[op.register]
				if !ok {
					return nil, 0, 0, fmt.Errorf("PUT with nothing cut into that register — a CUT in the same patch fills it")
				}
				e := at(from)
				if e.pre != nil {
					return nil, 0, 0, fmt.Errorf("line %d has two inserts above it in one patch", from)
				}
				e.pre = held
			case "SWAP", "SWAP.BLK":
				if err := cut(from, to); err != nil {
					return nil, 0, 0, err
				}
				e := at(from)
				e.deleted, e.swapped, e.swap = false, true, op.payload
			case "DEL", "DEL.BLK":
				if err := cut(from, to); err != nil {
					return nil, 0, 0, err
				}
			case "INS.PRE", "INS.HEAD", "INS.BLK.PRE":
				e := at(from)
				if e.pre != nil {
					return nil, 0, 0, fmt.Errorf("line %d has two inserts above it in one patch", from)
				}
				e.pre = op.payload
			case "INS.POST", "INS.TAIL", "INS.BLK.POST", "INS.BLK":
				e := at(to)
				if e.post != nil {
					return nil, 0, 0, fmt.Errorf("line %d has two inserts below it in one patch", to)
				}
				e.post = op.payload
			}
		}
	}

	out := make([]string, 0, len(lines))
	outFirst, outLast := 1, 0
	for i, line := range lines {
		e := edits[i+1]
		if i+1 == minLine {
			outFirst = len(out) + 1
		}
		if e == nil {
			out = append(out, line)
		} else {
			out = append(out, e.pre...)
			switch {
			case e.deleted:
			case e.swapped:
				out = append(out, e.swap...)
			default:
				out = append(out, line)
			}
			out = append(out, e.post...)
		}
		if i+1 == maxLine {
			outLast = len(out)
		}
	}
	if outLast < outFirst {
		outLast = outFirst
	}
	if outLast > len(out) {
		outLast = len(out)
	}
	return out, outFirst, outLast, nil
}

// checkFilePath catches a model swapping which argument holds the path and
// which holds the file's body — the `file` field landing with the whole
// content in it, say. Handed straight to the OS, that turns into an
// unreadable multi-kilobyte error (the bad "path" echoed back in full)
// instead of feedback the model can act on, so it is refused here first.
func checkFilePath(file string) error {
	const maxPathLen = 400
	if strings.ContainsAny(file, "\n\r") || len(file) > maxPathLen {
		shown := file
		if len(shown) > 60 {
			shown = shown[:60] + "…"
		}
		return fmt.Errorf("%q is not a file path — it's too long or has a newline in it; did the file content end up in the wrong argument?", shown)
	}
	return nil
}

type hashlineReadArgs struct {
	File      string `json:"file"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

func execHashlineRead(_ context.Context, call llm.ToolCall) (string, error) {
	var args hashlineReadArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "", fmt.Errorf("read: parse args: %w", err)
	}
	args.File = expandHome(args.File)
	if err := checkFilePath(args.File); err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	b, err := os.ReadFile(args.File)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	lines := splitLines(string(b))
	first, last := 1, len(lines)
	if args.StartLine > 0 {
		first = args.StartLine
	}
	if args.EndLine > 0 {
		last = args.EndLine
	}
	if first < 1 {
		first = 1
	}
	if last > len(lines) {
		last = len(lines)
	}
	if first > last {
		return "", fmt.Errorf("read: start_line %d after end_line %d (file has %d lines)", first, last, len(lines))
	}
	return renderHashlineRange(args.File, lines, first, last), nil
}

type hashlineOutlineArgs struct {
	File string `json:"file"`
}

// execHashlineOutline shows just the lines that look like declarations, with
// the same LINE:HASH anchors a full read would give them — so a model can
// scan a file's shape, then read or find_block around one signature instead
// of pulling the whole file into context to find it.
func execHashlineOutline(_ context.Context, call llm.ToolCall) (string, error) {
	var args hashlineOutlineArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "", fmt.Errorf("outline: parse args: %w", err)
	}
	args.File = expandHome(args.File)
	if err := checkFilePath(args.File); err != nil {
		return "", fmt.Errorf("outline: %w", err)
	}
	b, err := os.ReadFile(args.File)
	if err != nil {
		return "", fmt.Errorf("outline: %w", err)
	}
	lines := splitLines(string(b))
	var nums []int
	for i, line := range lines {
		if outlineSignature.MatchString(line) {
			nums = append(nums, i+1)
		}
	}
	if len(nums) == 0 {
		return fmt.Sprintf("[%s#%s]\nno recognized function/type/class signatures found — read the file instead", args.File, lineHash(strings.Join(lines, "\n"))), nil
	}
	return renderHashlineLines(args.File, lines, nums), nil
}

type hashlinePatchArgs struct {
	File   string `json:"file"`
	Patch  string `json:"patch"`
	DryRun bool   `json:"dry_run"`
}

// execHashlinePatch applies a patch and hands back the file as it now reads,
// hashes and all: every anchor the model was holding is stale the moment an
// edit lands, and a re-read is the only way to get the next one.
func execHashlinePatch(_ context.Context, call llm.ToolCall) (string, error) {
	var args hashlinePatchArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "", fmt.Errorf("patch: parse args: %w", err)
	}
	args.File = expandHome(args.File)
	if err := checkFilePath(args.File); err != nil {
		return "", fmt.Errorf("patch: %w", err)
	}
	info, err := os.Stat(args.File)
	if err != nil {
		return "", fmt.Errorf("patch: %w", err)
	}
	b, err := os.ReadFile(args.File)
	if err != nil {
		return "", fmt.Errorf("patch: %w", err)
	}
	ops, err := parseHashlinePatch(args.Patch)
	if err != nil {
		return "", fmt.Errorf("patch: %w", err)
	}
	orig := splitLines(string(b))
	out, first, last, err := applyHashline(orig, ops)
	if err != nil {
		return "", fmt.Errorf("patch: %w", err)
	}
	// A patch that leaves the file exactly as it was is a model copying the
	// line it read back into the replacement instead of correcting it, and
	// answering that with "Patch applied." feeds the loop: it re-reads, sees
	// the line unchanged, and sends the same copy again.
	if slices.Equal(orig, out) {
		return "", fmt.Errorf("patch: nothing changed — every replacement is identical to the line it replaces. Write the corrected line, not a copy of the one you read")
	}
	if args.DryRun {
		return "Dry-run result:\n" + renderHashlineRange(args.File, out, first, last), nil
	}
	// The file's own mode, not a fresh one: patching an executable must not
	// quietly take its bit away.
	if err := os.WriteFile(args.File, []byte(strings.Join(out, "\n")), info.Mode().Perm()); err != nil {
		return "", fmt.Errorf("patch: %w", err)
	}
	return "Patch applied.\n" + renderHashlineRange(args.File, out, first, last), nil
}

// hashlinePatchDescription spells the format out with a worked example. The
// hashline MCP server describes its patch tool as "Apply a hashline patch
// (SWAP, DEL, INS.* operations)" and nothing more, which leaves a model to
// guess — and what it guesses is unified diff, every time.
const hashlinePatchDescription = `Edit a file by line number and hash. Every operation names a line as LINE:HASH, exactly as the read tool printed it, and is refused if that line has changed since — so read the file first, and read it again after every patch.

Operations, one per line, each followed by its replacement lines:

  SWAP 12:a3f1        replace line 12 with the lines below it
  SWAP 12:a3f1..15    replace lines 12 to 15 with the lines below it
  DEL 15:9c2b         delete line 15, no lines below it
  DEL 15:9c2b..18     delete lines 15 to 18
  INS.PRE 20:41ff     insert the lines below it above line 20
  INS.POST 20:41ff    insert the lines below it under line 20
  INS.HEAD            insert the lines below it at the top of the file
  INS.TAIL            insert the lines below it at the bottom of the file
  SWAP.BLK 13:77c0    replace the whole block line 13 sits in
  DEL.BLK 13:77c0     delete the whole block line 13 sits in
  INS.BLK.PRE 13:77c0   insert the lines below it above that block
  INS.BLK.POST 13:77c0  insert the lines below it after that block
  INS.BLK 13:77c0     same as INS.BLK.POST
  CUT 12:a3f1..15 @b  lift lines 12 to 15 out and hold them as @b
  PUT @b <30:2e10     paste what @b holds above line 30

The block of a .BLK operation is the one the find_block tool would print for the same anchor. A CUT and the PUT that pastes it move code without repeating it; @name may be left off if the patch cuts only once, and a PUT with no <LINE pastes at the top of the file.

Replacement lines are marked with a leading + , as in the hashline CLI. Example patch, replacing one line with two and deleting another:

  SWAP 4:81e0
  +	println("one")
  +	println("two")
  DEL 7:c41a

The + is stripped before the line is written; write ++ for a line that really starts with a plus. A body with no + on any line is taken exactly as it stands, so mark either every line of an operation or none of them.

Line numbers all refer to the file as the read tool showed it, so operations do not have to be in order and do not have to account for lines added or removed by the ones before them. No two operations may touch the same line. Every anchor must carry its own hash — DEL 7 with no hash is refused, because nothing would check that line 7 is still what was read — and that applies to every operation in the patch, not just the first: SWAP 12:a3f1 followed by DEL 20 is refused on the DEL just as it would be alone. This is not unified diff: no @@ headers, and no - lines.

The result — dry-run or applied — shows only the lines the patch actually touched, not the whole file. Anchors outside that span are unchanged from the last read; anchors inside it are stale and need a fresh read (the whole file, or just that range with read's start_line/end_line) before the next patch touches them.`

type hashlineWriteArgs struct {
	File    string `json:"file"`
	Content string `json:"content"`
	Force   bool   `json:"force"`
}

// execHashlineWrite creates a file, or replaces one wholesale. Overwriting
// needs force: a model that means to edit a file it has read should patch it,
// and one that writes over it by accident loses everything it did not repeat.
func execHashlineWrite(_ context.Context, call llm.ToolCall) (string, error) {
	var args hashlineWriteArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "", fmt.Errorf("write: parse args: %w", err)
	}
	args.File = expandHome(args.File)
	if err := checkFilePath(args.File); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	if _, err := os.Stat(args.File); err == nil && !args.Force {
		return "", fmt.Errorf("write: %s exists — patch it, or pass force to replace all of it", args.File)
	}
	// An existing file keeps its own mode here: WriteFile only applies this
	// one when it has to create the file.
	if err := os.WriteFile(args.File, []byte(args.Content), 0o644); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	lines := splitLines(args.Content)
	return fmt.Sprintf("Wrote %s (%d lines).", args.File, len(lines)), nil
}

type hashlineFindBlockArgs struct {
	File   string `json:"file"`
	Anchor string `json:"anchor"`
}

// execHashlineFindBlock prints the block around an anchor and nothing else,
// so a model can see the whole of what it is about to edit without paying for
// the whole file again.
func execHashlineFindBlock(_ context.Context, call llm.ToolCall) (string, error) {
	var args hashlineFindBlockArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "", fmt.Errorf("find_block: parse args: %w", err)
	}
	args.File = expandHome(args.File)
	if err := checkFilePath(args.File); err != nil {
		return "", fmt.Errorf("find_block: %w", err)
	}
	b, err := os.ReadFile(args.File)
	if err != nil {
		return "", fmt.Errorf("find_block: %w", err)
	}
	lines := splitLines(string(b))
	n, err := resolveAnchor(lines, args.Anchor)
	if err != nil {
		return "", fmt.Errorf("find_block: %w", err)
	}
	first, last := findBlock(lines, n)
	return renderHashlineRange(args.File, lines, first, last), nil
}

type hashlineRemoveArgs struct {
	File string `json:"file"`
}

func execHashlineRemove(_ context.Context, call llm.ToolCall) (string, error) {
	var args hashlineRemoveArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "", fmt.Errorf("remove_file: parse args: %w", err)
	}
	args.File = expandHome(args.File)
	if err := checkFilePath(args.File); err != nil {
		return "", fmt.Errorf("remove_file: %w", err)
	}
	if err := os.Remove(args.File); err != nil {
		return "", fmt.Errorf("remove_file: %w", err)
	}
	return "removed " + args.File, nil
}

type hashlineRenameArgs struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
}

// execHashlineRename refuses to rename onto an existing file: os.Rename would
// replace it without a word, and nothing here would know what was lost.
func execHashlineRename(_ context.Context, call llm.ToolCall) (string, error) {
	var args hashlineRenameArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "", fmt.Errorf("rename_file: parse args: %w", err)
	}
	args.Src, args.Dst = expandHome(args.Src), expandHome(args.Dst)
	if err := checkFilePath(args.Src); err != nil {
		return "", fmt.Errorf("rename_file: %w", err)
	}
	if err := checkFilePath(args.Dst); err != nil {
		return "", fmt.Errorf("rename_file: %w", err)
	}
	if _, err := os.Stat(args.Dst); err == nil {
		return "", fmt.Errorf("rename_file: %s exists — remove it first if it is meant to go", args.Dst)
	}
	if err := os.Rename(args.Src, args.Dst); err != nil {
		return "", fmt.Errorf("rename_file: %w", err)
	}
	return "renamed " + args.Src + " to " + args.Dst, nil
}
