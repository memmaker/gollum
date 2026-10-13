package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memmaker/gollum/llm"
)

// callTool runs one of the tool executors the way the session does, with its
// arguments as the JSON blob a model would have produced.
func callTool(t *testing.T, fn func(context.Context, llm.ToolCall) (string, error), args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := fn(context.Background(), llm.ToolCall{Function: llm.ToolCallFunction{Arguments: string(raw)}})
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	return out
}

// hlFile is a small file to edit, and the anchors its lines actually have.
func hlFile() []string {
	return splitLines("package main\n\nfunc main() {\n\tprintln(\"one\")\n\tprintln(\"two\")\n}\n")
}

// anchor is the LINE:HASH a patch has to quote for line n, so the tests read
// the way a model's patch does without hard-coding hashes that a change to the
// hash function would invalidate everywhere.
func anchor(lines []string, n int) string {
	return strings.TrimSpace(strings.Split(strings.Split(renderHashline("f", lines), "\n")[n], "|")[0])
}

func patched(t *testing.T, lines []string, patch string) []string {
	t.Helper()
	ops, err := parseHashlinePatch(patch)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, _, _, err := applyHashline(lines, ops)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return out
}

// Anchors are resolved against the file as it was read, so an operation that
// grows the file does not move the line every later operation is aiming at.
func TestApplyResolvesAnchorsAgainstTheOriginal(t *testing.T) {
	lines := hlFile()
	out := patched(t, lines, "SWAP "+anchor(lines, 4)+"\n\tA1\n\tA2\n\tA3\nSWAP "+anchor(lines, 5)+"\n\tB\n")

	want := "package main\n\nfunc main() {\n\tA1\n\tA2\n\tA3\n\tB\n}\n"
	if got := strings.Join(out, "\n"); got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

// Same reason: the operations of a patch carry their own line numbers, so the
// order they are written in cannot matter.
func TestApplyIsOrderIndependent(t *testing.T) {
	lines := hlFile()
	up := patched(t, lines, "SWAP "+anchor(lines, 3)+"\nHEAD\nSWAP "+anchor(lines, 5)+"\nTAIL\n")
	down := patched(t, lines, "SWAP "+anchor(lines, 5)+"\nTAIL\nSWAP "+anchor(lines, 3)+"\nHEAD\n")
	if strings.Join(up, "\n") != strings.Join(down, "\n") {
		t.Fatalf("the same edits in two orders gave two files:\n%q\n%q", up, down)
	}
	if !strings.Contains(strings.Join(up, "\n"), "HEAD\n\tprintln(\"one\")\nTAIL") {
		t.Fatalf("edits did not land where they were aimed:\n%q", strings.Join(up, "\n"))
	}
}

func TestInsertAroundALine(t *testing.T) {
	lines := hlFile()
	out := patched(t, lines, "INS.PRE "+anchor(lines, 4)+"\nabove\nINS.POST "+anchor(lines, 4)+"\nbelow\n")
	want := "package main\n\nfunc main() {\nabove\n\tprintln(\"one\")\nbelow\n\tprintln(\"two\")\n}\n"
	if got := strings.Join(out, "\n"); got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestDeleteALine(t *testing.T) {
	lines := hlFile()
	out := patched(t, lines, "DEL "+anchor(lines, 4)+"\n")
	if got := strings.Join(out, "\n"); strings.Contains(got, "one") {
		t.Fatalf("line 4 survived its deletion:\n%q", got)
	}
}

// The whole point of the hashes: an anchor that no longer describes the line
// it names is refused rather than applied to whatever moved into that slot.
func TestStaleAnchorIsRefused(t *testing.T) {
	lines := hlFile()
	ops, err := parseHashlinePatch("SWAP 4:0000\nnope\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, _, _, err := applyHashline(lines, ops); err == nil {
		t.Fatal("a patch quoting the wrong hash was applied anyway")
	} else if !strings.Contains(err.Error(), "changed since it was read") {
		t.Fatalf("unhelpful error for a stale anchor: %v", err)
	}
}

// The bug this implementation exists to not have: the hashline MCP server
// folds a misspelled operation into the previous one's replacement lines and
// writes it into the file as text, without saying so, as long as some other
// operation in the same patch succeeded.
func TestUnknownOperationIsRefusedNotWritten(t *testing.T) {
	lines := hlFile()
	_, err := parseHashlinePatch("SWAP " + anchor(lines, 4) + "\n\tkept\nBOGUS.OP " + anchor(lines, 5) + "\n\tsilently dropped\n")
	if err == nil {
		t.Fatal("a patch naming an unknown operation was accepted")
	}
	if !strings.Contains(err.Error(), "BOGUS.OP") {
		t.Fatalf("error does not name the operation it rejected: %v", err)
	}
}

// Unified diff is what a model reaches for when it is unsure, so it has to
// fail loudly rather than be taken for replacement text.
func TestUnifiedDiffIsRefused(t *testing.T) {
	if _, err := parseHashlinePatch("@@ -4,1 +4,1 @@\n-\tprintln(\"one\")\n+\tprintln(\"uno\")\n"); err == nil {
		t.Fatal("a unified diff was accepted as a hashline patch")
	}
}

func TestTwoEditsOnOneLineAreRefused(t *testing.T) {
	lines := hlFile()
	ops, err := parseHashlinePatch("SWAP " + anchor(lines, 4) + "\nX\nSWAP " + anchor(lines, 4) + "\nY\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, _, _, err := applyHashline(lines, ops); err == nil {
		t.Fatal("two edits to the same line were both applied")
	}
}

func TestDelTakesNoPayload(t *testing.T) {
	if _, err := parseHashlinePatch("DEL 4:81e0\nreplacement\n"); err == nil {
		t.Fatal("DEL accepted replacement lines")
	}
}

// Reading a file and joining it back has to reproduce it exactly, trailing
// newline included, or every patch quietly rewrites the end of the file.
func TestSplitLinesRoundTrips(t *testing.T) {
	for _, s := range []string{"a\nb\n", "a\nb", "", "\n", "no newline"} {
		if got := strings.Join(splitLines(s), "\n"); got != s {
			t.Errorf("round trip of %q gave %q", s, got)
		}
	}
}

// The mirror image of TestTwoEditsOnOneLineAreRefused's neighbor: a model
// that quotes the hash a read printed but drops the "LINE:" it was attached
// to gets pointed at the fix, not a generic "not valid" error.
func TestHashOnlyAnchorNamesTheMissingPiece(t *testing.T) {
	_, err := parseHashlinePatch("SWAP 8e4f..73d7:\nreplacement\n")
	if err == nil {
		t.Fatal("a hash-only anchor was accepted")
	}
	if !strings.Contains(err.Error(), "missing its line number") {
		t.Fatalf("error did not name the actual mistake: %v", err)
	}
}

// A model that copies the line it read into the replacement instead of
// correcting it must be told so. Answering that with "Patch applied." is what
// turns one failed edit into a loop of them.
func TestPatchThatChangesNothingIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hl.go")
	lines := hlFile()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := "SWAP " + anchor(lines, 4) + "\n" + lines[3] + "\n"

	raw, err := json.Marshal(map[string]any{"file": path, "patch": patch})
	if err != nil {
		t.Fatal(err)
	}
	_, err = execHashlinePatch(context.Background(), llm.ToolCall{Function: llm.ToolCallFunction{Arguments: string(raw)}})
	if err == nil {
		t.Fatal("a patch that changed nothing reported success")
	}
	if !strings.Contains(err.Error(), "nothing changed") {
		t.Fatalf("error did not name the actual mistake: %v", err)
	}
}

func TestOutlineListsSignaturesNotBodies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hl.go")
	if err := os.WriteFile(path, []byte(strings.Join(hlFile(), "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	out := callTool(t, execHashlineOutline, map[string]any{"file": path})
	if !strings.Contains(out, "func main()") {
		t.Fatalf("outline missed the func signature:\n%s", out)
	}
	if strings.Contains(out, "println") {
		t.Fatalf("outline included a body line:\n%s", out)
	}
}

// The end to end path, through the tools the model actually calls.
func TestPatchToolWritesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hl.go")
	if err := os.WriteFile(path, []byte(strings.Join(hlFile(), "\n")), 0o755); err != nil {
		t.Fatal(err)
	}

	read := callTool(t, execHashlineRead, map[string]any{"file": path})
	if !strings.Contains(read, "4:") || !strings.Contains(read, `println("one")`) {
		t.Fatalf("read did not number the file:\n%s", read)
	}

	ranged := callTool(t, execHashlineRead, map[string]any{"file": path, "start_line": float64(4), "end_line": float64(4)})
	if !strings.Contains(ranged, `println("one")`) || strings.Contains(ranged, "1:") {
		t.Fatalf("ranged read did not limit to line 4:\n%s", ranged)
	}

	patch := "SWAP " + anchor(hlFile(), 4) + "\n\tprintln(\"uno\")\n"
	out := callTool(t, execHashlinePatch, map[string]any{"file": path, "patch": patch, "dry_run": true})
	if !strings.Contains(out, "uno") {
		t.Fatalf("dry run did not show the result:\n%s", out)
	}
	if strings.Contains(out, "package main") {
		t.Fatalf("dry run echoed untouched lines instead of just the edit:\n%s", out)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "uno") {
		t.Fatal("a dry run wrote to the file")
	}

	callTool(t, execHashlinePatch, map[string]any{"file": path, "patch": patch, "dry_run": false})
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `println("uno")`) || strings.Contains(string(b), `println("one")`) {
		t.Fatalf("the patch did not land:\n%s", b)
	}
	// An executable that gets patched stays executable.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("file mode is now %v, want 0755", info.Mode().Perm())
	}
}

// A range covers every line between its two anchors, so replacing four lines
// with one is one operation rather than a SWAP and three DELs.
func TestSwapARange(t *testing.T) {
	lines := hlFile()
	out := patched(t, lines, "SWAP "+anchor(lines, 4)+".."+anchor(lines, 5)+"\n\tprintln(\"merged\")\n")

	want := "package main\n\nfunc main() {\n\tprintln(\"merged\")\n}\n"
	if got := strings.Join(out, "\n"); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestDeleteARange(t *testing.T) {
	lines := hlFile()
	out := patched(t, lines, "DEL "+anchor(lines, 4)+".."+anchor(lines, 5)+"\n")

	want := "package main\n\nfunc main() {\n}\n"
	if got := strings.Join(out, "\n"); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// The ends of the file need no anchor: there is nothing there to have changed.
func TestInsertAtHeadAndTail(t *testing.T) {
	lines := hlFile()
	out := patched(t, lines, "INS.HEAD\n// top\nINS.TAIL\n// bottom\n")

	if out[0] != "// top" || out[len(out)-1] != "// bottom" {
		t.Fatalf("got:\n%s", strings.Join(out, "\n"))
	}
}

// A .BLK operation takes the block its anchor sits in — here the whole
// function, reached from a line in the middle of its body.
func TestBlockOperationsUseTheEnclosingBlock(t *testing.T) {
	lines := hlFile()
	if first, last := findBlock(lines, 4); first != 3 || last != 6 {
		t.Fatalf("findBlock(4) = %d..%d, want 3..6", first, last)
	}

	out := patched(t, lines, "SWAP.BLK "+anchor(lines, 4)+"\nfunc main() {}\n")
	want := "package main\n\nfunc main() {}\n"
	if got := strings.Join(out, "\n"); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// CUT and PUT move lines without repeating them, and the PUT may be written
// first: both anchors point at the file as it was read.
func TestCutAndPutMoveLines(t *testing.T) {
	lines := hlFile()
	out := patched(t, lines, "PUT @body <"+anchor(lines, 3)+"\nCUT "+anchor(lines, 4)+".."+anchor(lines, 5)+" @body\n")

	want := "package main\n\n\tprintln(\"one\")\n\tprintln(\"two\")\nfunc main() {\n}\n"
	if got := strings.Join(out, "\n"); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestPutWithoutCutIsRefused(t *testing.T) {
	lines := hlFile()
	ops, err := parseHashlinePatch("PUT @body <" + anchor(lines, 3) + "\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, _, _, err := applyHashline(lines, ops); err == nil {
		t.Fatal("a PUT with an empty register was applied")
	}
}

// The CLI's envelope markers are stripped rather than written into the file.
func TestEnvelopeMarkersAreStripped(t *testing.T) {
	lines := hlFile()
	out := patched(t, lines, "*** Begin Patch\n[f#0000]\nSWAP "+anchor(lines, 4)+"\n\tA\n*** End Patch\n")

	if strings.Contains(strings.Join(out, "\n"), "***") {
		t.Fatalf("markers written into the file:\n%s", strings.Join(out, "\n"))
	}
	if out[3] != "\tA" {
		t.Fatalf("line 4 = %q, want the replacement", out[3])
	}
}

func TestAbortAppliesNothing(t *testing.T) {
	lines := hlFile()
	if _, err := parseHashlinePatch("*** Begin Patch\nSWAP " + anchor(lines, 4) + "\n\tA\n*** Abort\n"); err == nil {
		t.Fatal("an aborted patch was parsed")
	}
}

// The CLI marks its replacement lines with a leading +, and a model that
// knows that server writes them that way. The marker is stripped, not written.
func TestCliPlusPrefixesAreStripped(t *testing.T) {
	lines := hlFile()
	out := patched(t, lines, "SWAP "+anchor(lines, 4)+":\n+\tprintln(\"plussed\")\n")

	if out[3] != "\tprintln(\"plussed\")" {
		t.Fatalf("line 4 = %q, want the + stripped", out[3])
	}
}

// A body with no markers is written exactly as it stands, and ++ is how a
// line that really starts with a + gets through the stripping.
func TestDoublePlusWritesALiteralPlus(t *testing.T) {
	lines := hlFile()
	out := patched(t, lines, "SWAP "+anchor(lines, 4)+"\nx + y\n")
	if out[3] != "x + y" {
		t.Fatalf("line 4 = %q, want an unmarked body left alone", out[3])
	}

	out = patched(t, lines, "SWAP "+anchor(lines, 4)+"\n+A\n++x\n")
	if out[3] != "A" || out[4] != "+x" {
		t.Fatalf("lines = %q, want one + taken off each", out[3:5])
	}
}

// Half-marked is ambiguous: either half could be the content.
func TestMixedPlusPrefixesAreRefused(t *testing.T) {
	lines := hlFile()
	if _, err := parseHashlinePatch("SWAP " + anchor(lines, 4) + "\n+A\nB\n"); err == nil {
		t.Fatal("a half-marked body was accepted")
	}
}

// A line number with no hash is what the CLI takes on trust. Here it is an
// error, not a line of content to write into the file.
func TestAnchorWithoutHashIsRefused(t *testing.T) {
	if _, err := parseHashlinePatch("DEL 4\n"); err == nil {
		t.Fatal("an unhashed anchor was accepted")
	}
}

// Patching straight off a compiler error gives line numbers and no hashes at
// all. Every spelling of that has to say so, and point at read.
func TestUnhashedRangeSaysToReadTheFile(t *testing.T) {
	for _, patch := range []string{"SWAP 321:..325:\n+x\n", "SWAP 321..325\n+x\n", "SWAP 321:\n+x\n"} {
		_, err := parseHashlinePatch(patch)
		if err == nil {
			t.Fatalf("%q was accepted", patch)
		}
		if !strings.Contains(err.Error(), "has no hash") || !strings.Contains(err.Error(), "read the file") {
			t.Fatalf("%q got unhelpful error: %v", patch, err)
		}
	}
}

func TestFindBlockToolPrintsTheEnclosingBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	lines := hlFile()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		t.Fatal(err)
	}

	out := callTool(t, execHashlineFindBlock, map[string]any{"file": path, "anchor": anchor(lines, 4)})
	if !strings.Contains(out, "3:") || !strings.Contains(out, "6:") || strings.Contains(out, "1:") {
		t.Fatalf("block = %q, want lines 3 to 6 only", out)
	}
}

func TestWriteRefusesToOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	if err := os.WriteFile(path, []byte("keep me\n"), 0600); err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal(map[string]any{"file": path, "content": "gone\n"})
	if _, err := execHashlineWrite(context.Background(), llm.ToolCall{Function: llm.ToolCallFunction{Arguments: string(raw)}}); err == nil {
		t.Fatal("write clobbered an existing file")
	}
	if b, _ := os.ReadFile(path); string(b) != "keep me\n" {
		t.Fatalf("file = %q, want it untouched", string(b))
	}

	callTool(t, execHashlineWrite, map[string]any{"file": path, "content": "gone\n", "force": true})
	if b, _ := os.ReadFile(path); string(b) != "gone\n" {
		t.Fatalf("file = %q, want the forced write", string(b))
	}
}

// A model naming a path with ~ hits Go's raw file APIs, which never expand
// it — write used to fail as if the file just didn't exist.
func TestWriteExpandsHomeDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir to test against")
	}
	dir, err := os.MkdirTemp(home, "hashline-home-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	rel := "~/" + filepath.Base(dir) + "/f.go"
	callTool(t, execHashlineWrite, map[string]any{"file": rel, "content": "package main\n"})
	if b, err := os.ReadFile(filepath.Join(dir, "f.go")); err != nil || string(b) != "package main\n" {
		t.Fatalf("write did not expand %q: %v, %q", rel, err, b)
	}
}

// write's whole point is creating content the model already wrote — echoing
// it back doubles the tokens for nothing.
func TestWriteReturnsASummaryNotTheContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	content := "package main\n\nfunc main() {}\n"
	out := callTool(t, execHashlineWrite, map[string]any{"file": path, "content": content})
	if strings.Contains(out, "func main") {
		t.Fatalf("write echoed the file content back:\n%s", out)
	}
	if !strings.Contains(out, path) {
		t.Fatalf("write's summary does not name the file:\n%s", out)
	}
}

// The file tools take whatever os.ReadFile/os.WriteFile would: a relative
// path resolves against the process's working directory, same as any CLI.
func TestFileToolsAcceptRelativePaths(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	callTool(t, execHashlineWrite, map[string]any{"file": "rel.go", "content": "package main\n"})
	if b, err := os.ReadFile(filepath.Join(dir, "rel.go")); err != nil || string(b) != "package main\n" {
		t.Fatalf("write did not create the file relative to cwd: %v, %q", err, b)
	}

	read := callTool(t, execHashlineRead, map[string]any{"file": "rel.go"})
	if !strings.Contains(read, "package main") {
		t.Fatalf("read did not find the file relative to cwd:\n%s", read)
	}

	abs := callTool(t, execHashlineRead, map[string]any{"file": filepath.Join(dir, "rel.go")})
	if !strings.Contains(abs, "package main") {
		t.Fatalf("read did not find the file by absolute path:\n%s", abs)
	}
}

func TestRenameAndRemove(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(src, []byte("x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	callTool(t, execHashlineRename, map[string]any{"src": src, "dst": dst})
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// Renaming onto a file that exists would lose it without a word.
	if err := os.WriteFile(src, []byte("y\n"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"src": src, "dst": dst})
	if _, err := execHashlineRename(context.Background(), llm.ToolCall{Function: llm.ToolCallFunction{Arguments: string(raw)}}); err == nil {
		t.Fatal("rename overwrote an existing file")
	}

	callTool(t, execHashlineRemove, map[string]any{"file": dst})
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("remove left the file behind")
	}
}
