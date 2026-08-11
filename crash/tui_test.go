package main

import (
	"context"
	"strings"
	"testing"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/memmaker/gollum/llm"
)

// The scrollbar is the only arithmetic in the view: a thumb that leaves the
// track, or a track of the wrong height, breaks the whole layout.
func TestScrollbar(t *testing.T) {
	vp := viewport.New(20, 10)
	vp.SetContent(strings.Repeat("line\n", 5))
	if got := strings.Count(scrollbar(vp, true), "\n") + 1; got != 10 {
		t.Fatalf("short content: got %d rows, want 10", got)
	}
	// Content that fits fills the bar: with no borders left, the scrollbar is
	// what shows which pane has focus.
	if got := strings.Count(scrollbar(vp, true), scrollbarThumb); got != 10 {
		t.Fatalf("short content drew %d thumb rows, want a full 10", got)
	}
	lipgloss.SetColorProfile(termenv.TrueColor)
	if scrollbar(vp, true) == scrollbar(vp, false) {
		t.Fatal("focused and unfocused scrollbars look the same")
	}

	vp.SetContent(strings.Repeat("line\n", 100))
	for _, at := range []func(){func() { vp.GotoTop() }, func() { vp.GotoBottom() }} {
		at()
		rows := strings.Split(scrollbar(vp, true), "\n")
		if len(rows) != 10 {
			t.Fatalf("got %d rows, want 10", len(rows))
		}
		thumb := 0
		for _, r := range rows {
			if strings.Contains(r, scrollbarThumb) {
				thumb++
			}
		}
		if thumb < 1 || thumb > 10 {
			t.Fatalf("thumb of %d rows escapes the track", thumb)
		}
	}
	if !strings.Contains(strings.Split(scrollbar(vp, true), "\n")[9], scrollbarThumb) {
		t.Fatal("at the bottom the thumb should reach the last row")
	}
}

// The overlay must sit inside the frame without pushing rows or columns
// around: a modal that resizes the screen means the compositing is wrong.
func TestChoiceOverlay(t *testing.T) {
	m := &tui{
		sess:    llm.NewSession(nil, ""),
		cfgs:    []llm.Config{{Provider: "openai", Model: "gpt-5-mini"}},
		oneLine: newOneLineInput(), multiLine: newMultiLineInput(),
		help:    help.New(),
		current: "openai/gpt-5-mini",
	}
	m.layout(100, 30)
	plain := m.View()

	m.openChoice("available models", []string{"openai/a", "openai/b"}, func(int) {})
	withModal := m.View()

	if lipgloss.Height(plain) != lipgloss.Height(withModal) {
		t.Fatalf("overlay changed the frame height: %d then %d",
			lipgloss.Height(plain), lipgloss.Height(withModal))
	}
	if lipgloss.Width(plain) != lipgloss.Width(withModal) {
		t.Fatalf("overlay changed the frame width: %d then %d",
			lipgloss.Width(plain), lipgloss.Width(withModal))
	}
	if !strings.Contains(withModal, "available models") {
		t.Fatal("overlay title is not on screen")
	}

	// Cancelling must take the overlay back off without touching the frame.
	got := 0
	m.onChoice = func(i int) { got = i }
	m.closeChoice(-1)
	if got != -1 || m.choice != nil {
		t.Fatalf("cancel left the overlay up (choice=%v, index=%d)", m.choice, got)
	}
	if m.View() != plain {
		t.Fatal("closing the overlay did not restore the plain view")
	}
}

// Filtering runs as a command whose result comes back as a message, and it
// renumbers the rows: both are easy to drop, and both make the wrong item win.
func TestChoiceFilterAndPick(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), cfgs: []llm.Config{{Provider: "p"}}, oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New()}
	m.layout(100, 30)

	picked := -99
	options := []string{"openai/gpt-5", "gemini/flash", "openai/o3"}
	m.openChoice("models", options, func(i int) { picked = i })

	// Type "/gem", then feed back whatever the filter command produced.
	send := func(r rune) tea.Cmd { return m.updateChoice(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}) }
	send('/')
	var cmd tea.Cmd
	for _, r := range "gem" {
		cmd = send(r)
	}
	m.run(t, cmd)
	if got := len(m.choice.VisibleItems()); got != 1 {
		t.Fatalf("filter matched %d items, want 1", got)
	}

	m.updateChoice(tea.KeyMsg{Type: tea.KeyEnter}) // apply the filter
	m.updateChoice(tea.KeyMsg{Type: tea.KeyEnter}) // choose the only match
	if picked != 1 {
		t.Fatalf("picked index %d, want 1 (%q)", picked, options[1])
	}
}

// run drains a command the way the bubbletea event loop would, unwrapping the
// batches the list hands back.
func (m *tui) run(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	for depth := 0; cmd != nil && depth < 3; depth++ {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			cmd = nil
			for _, c := range batch {
				m.run(t, c)
			}
			continue
		}
		if msg == nil {
			return
		}
		_, cmd = m.Update(msg)
	}
}

// Fuzzy matching turns a model search into noise: "fable" subsequence-matches
// a FLUX model. The filter has to mean substring.
func TestSubstringFilter(t *testing.T) {
	targets := []string{
		"omniroute/huggingface/black-forest-labs/FLUX.1-schnell",
		"omniroute/fable-5",
	}
	got := substringFilter("fable", targets)
	if len(got) != 1 || got[0].Index != 1 {
		t.Fatalf("filter matched %v, want only index 1", got)
	}
	if len(substringFilter("FABLE", targets)) != 1 {
		t.Fatal("filter should ignore case")
	}
}

// Five options belong on one page. The list subtracts its own title,
// pagination and help rows from the height it is given, so a box sized to the
// item count alone paginates one item at a time.
func TestChoiceFitsOnePage(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), cfgs: []llm.Config{{Provider: "p"}}, oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New()}
	m.layout(100, 30)

	m.openChoice("pick an animal", []string{"cat", "dog", "owl", "bat", "eel"}, func(int) {})
	if got := m.choice.Paginator.PerPage; got != 5 {
		t.Fatalf("%d of 5 options per page", got)
	}
	if got := m.choice.Paginator.TotalPages; got != 1 {
		t.Fatalf("5 options spread over %d pages", got)
	}

	// Past the cap the list is expected to paginate rather than eat the screen.
	many := make([]string, 60)
	for i := range many {
		many[i] = "model"
	}
	m.openChoice("models", many, func(int) {})
	if got := m.choice.Paginator.PerPage; got != maxChoiceRows {
		t.Fatalf("long list shows %d rows, want %d", got, maxChoiceRows)
	}
}

// Tool activity is drawn where it happened: after the text that preceded it,
// with the result under its call. /debug decides how much of each is shown.
func TestToolCallRendering(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), cfgs: []llm.Config{{Provider: "p"}}, oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New(), raw: true}
	m.layout(100, 30)

	long := `{"question":"pick an animal","options":["cat","dog","owl","bat","eel"]}`
	m.chunk(llm.Message{Content: "let me ask. "})
	m.chunk(llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
		{ID: "1", Function: llm.ToolCallFunction{Name: "user_choice", Arguments: long}},
	}})
	m.chunk(llm.Message{Role: llm.RoleTool, Name: "user_choice", Content: "cat"})

	out := m.transcript.String()
	if !strings.Contains(out, "user_choice") || !strings.Contains(out, "cat") {
		t.Fatalf("tool block missing from transcript:\n%s", out)
	}
	if strings.Index(out, "let me ask.") > strings.Index(out, "user_choice") {
		t.Fatal("tool call was drawn before the text that preceded it")
	}
	if strings.Contains(out, long) {
		t.Fatal("full arguments shown with debug off")
	}

	// A usage report is bookkeeping, not an answer.
	m.transcript.Reset()
	m.chunk(llm.Message{Usage: &llm.Usage{InputTokens: 12, OutputTokens: 3, TotalTokens: 15}})
	if m.transcript.Len() != 0 || m.answer.Len() != 0 {
		t.Fatalf("token usage leaked into the transcript: %q", m.transcript.String()+m.answer.String())
	}

	m.debug = true
	m.transcript.Reset()
	m.chunk(llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
		{ID: "1", Function: llm.ToolCallFunction{Name: "user_choice", Arguments: long}},
	}})
	if !strings.Contains(m.transcript.String(), long) {
		t.Fatal("debug mode still truncates the arguments")
	}
}

// Assistant text that resumes after a tool result must start its own block,
// whatever trailing newlines the previous writer happened to leave.
func TestAnswerStartsNewBlockAfterTool(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), cfgs: []llm.Config{{Provider: "p"}}, oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New(), raw: true}
	m.layout(100, 30)

	m.chunk(llm.Message{Role: llm.RoleTool, Name: "user_choice", Content: "cat"})
	m.transcript.Reset()
	m.transcript.WriteString("  ↳ cat") // no trailing newline at all
	m.chunk(llm.Message{Content: "You picked cat."})

	if got := m.transcript.String() + m.answer.String(); !strings.Contains(got, "cat\n\nYou picked") {
		t.Fatalf("answer ran into the tool result:\n%q", got)
	}
}

// The bar is one row of the frame and must stay that way at any width, or
// every pane below it shifts.
func TestAnimBar(t *testing.T) {
	for _, w := range []int{1, 40, 200} {
		for _, active := range []bool{false, true} {
			got := animBar(w, 7, active)
			if lipgloss.Width(got) != w {
				t.Fatalf("width %d active=%v rendered %d cells", w, active, lipgloss.Width(got))
			}
			if lipgloss.Height(got) != 1 {
				t.Fatalf("bar is %d rows tall", lipgloss.Height(got))
			}
		}
	}
	// Idle means invisible: no ink, but still one row wide.
	if strings.TrimSpace(animBar(40, 7, false)) != "" {
		t.Fatal("the idle bar draws something")
	}
	// The sweep reverses rather than wrapping: halfway through the second
	// cycle it is back where it was halfway through the first.
	if animBar(40, animCycle/2, true) != animBar(40, 2*animCycle-animCycle/2, true) {
		t.Fatal("the sweep does not ping-pong")
	}
	// The highlight has to move, or it is not an animation. Tests run without
	// a TTY, where lipgloss drops colour entirely, so ask for it explicitly.
	lipgloss.SetColorProfile(termenv.TrueColor)
	if animBar(40, 0, true) == animBar(40, 10, true) {
		t.Fatal("frame 0 and frame 10 render identically")
	}
}

// The menu entry both shows the toggle's state and flips it.
func TestMenuToggleSendTools(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), cfgs: []llm.Config{{Provider: "p"}}, oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New()}
	m.layout(100, 30)

	m.openMenu()
	if got := m.choice.VisibleItems()[0].(choiceItem).text; got != "Send tools: on" {
		t.Fatalf("menu shows %q, want the toggle to start on", got)
	}
	m.closeChoice(0)
	if m.sess.SendRegisteredTools {
		t.Fatal("choosing the entry did not turn tools off")
	}

	m.openMenu()
	if got := m.choice.VisibleItems()[0].(choiceItem).text; got != "Send tools: off" {
		t.Fatalf("menu shows %q, want it to report the new state", got)
	}
	m.closeChoice(0)
	if !m.sess.SendRegisteredTools {
		t.Fatal("the toggle does not go back on")
	}
}

// Copying has to yield plain text from one pane only, and the highlight has
// to survive a scroll (it is anchored to content lines, not screen rows).
func TestSelectionCopiesPlainLines(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), cfgs: []llm.Config{{Provider: "p"}}, oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New(), raw: true}
	m.layout(100, 30)
	m.transcript.WriteString(label.Render("styled one") + "\nplain two\nplain three\n")
	m.redraw()

	m.sel = selection{pane: paneOutput, from: 0, to: 1, set: true}
	m.copySelection()

	got, err := clipboard.ReadAll()
	if err != nil {
		t.Skip("no clipboard in this environment:", err)
	}
	if got != "styled one\nplain two" {
		t.Fatalf("clipboard holds %q", got)
	}
	if !strings.Contains(m.note, "copied") {
		t.Fatalf("no confirmation shown, note is %q", m.note)
	}

	// The sidebar keeps its own selection state.
	if m.sel.covers(paneSidebar, 0) {
		t.Fatal("an output selection bled into the sidebar")
	}
}

// The pager shows what was actually sent, tool calls included.
func TestRenderContext(t *testing.T) {
	out := renderContext([]llm.Message{
		{Role: llm.RoleUser, Content: "pick one"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{Function: llm.ToolCallFunction{Name: "user_choice", Arguments: `{"options":["cat"]}`}},
		}},
		{Role: llm.RoleTool, Name: "user_choice", Content: "cat"},
	})
	for _, want := range []string{"pick one", "user_choice", `{"options":["cat"]}`, "cat"} {
		if !strings.Contains(out, want) {
			t.Fatalf("context view is missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(renderContext(nil), "no request") {
		t.Fatal("an empty context should say so")
	}
}

// The pager selects and copies with the mouse like the panes do.
func TestPagerMouseSelection(t *testing.T) {
	p := newPager("context", "alpha\nbeta\ngamma", 60, 20)

	press := tea.MouseMsg{X: 3, Y: titleHeight, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}
	drag := tea.MouseMsg{X: 3, Y: titleHeight + 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion}
	release := tea.MouseMsg{X: 3, Y: titleHeight + 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease}
	p.mouse(press)
	p.mouse(drag)
	if !p.sel.covers(panePager, 0) || !p.sel.covers(panePager, 1) {
		t.Fatal("dragging did not extend the selection")
	}
	p.mouse(release)

	got, err := clipboard.ReadAll()
	if err != nil {
		t.Skip("no clipboard in this environment:", err)
	}
	if got != "alpha\nbeta" {
		t.Fatalf("clipboard holds %q", got)
	}
	if !strings.Contains(p.view(help.New()), "copied") {
		t.Fatal("the pager does not confirm the copy on screen")
	}

	// A keypress clears the highlight, as in the panes.
	p.update(tea.KeyMsg{Type: tea.KeyDown})
	if p.sel.set {
		t.Fatal("a keypress left the selection up")
	}
}

// A pane that fits its content and has no focus shows no bar at all, but keeps
// the column so nothing shifts when focus arrives.
func TestScrollbarHiddenWhenIdle(t *testing.T) {
	vp := viewport.New(10, 4)
	vp.SetContent("one\ntwo")

	hidden := scrollbar(vp, false)
	if strings.TrimSpace(hidden) != "" {
		t.Fatalf("idle scrollbar = %q, want blank", ansi.Strip(hidden))
	}
	if got := lipgloss.Width(hidden); got != 1 {
		t.Fatalf("idle scrollbar width = %d, want 1", got)
	}
	if got := lipgloss.Height(hidden); got != 4 {
		t.Fatalf("idle scrollbar height = %d, want 4", got)
	}
	// Focused, or scrollable, and it is drawn again.
	if strings.TrimSpace(ansi.Strip(scrollbar(vp, true))) == "" {
		t.Fatal("focused scrollbar is blank")
	}
	vp.SetContent("1\n2\n3\n4\n5\n6")
	if strings.TrimSpace(ansi.Strip(scrollbar(vp, false))) == "" {
		t.Fatal("scrollable pane shows no bar")
	}
}

// After the markdown pass the reader wants the question they asked at the top
// of the pane, not the tail of the answer.
func TestFinishTurnScrollsToPrompt(t *testing.T) {
	// A client pointing nowhere: the send fails immediately, which is all this
	// test needs — the echo and the turn's start offset are recorded either way.
	client, err := llm.New(llm.Config{Provider: "openai", APIKey: "x", BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	m := &tui{ctx: context.Background(), sess: llm.NewSession(client, ""), oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New(), raw: true}
	m.layout(100, 20)
	// An earlier turn, so the interesting offset is not zero.
	m.transcript.WriteString(strings.Repeat("old\n", 30))

	m.setValue("ask something")
	m.submit()
	m.produced = true
	m.chunk(llm.Message{Content: strings.Repeat("answer line\n", 40)})
	m.finishTurn()

	lines := strings.Split(ansi.Strip(m.out.View()), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "> ask something") {
		t.Fatalf("top line = %q, want the prompt", lines[0])
	}
}

// A toggle leaves the menu up with its label refreshed, so it can be flipped
// again without reopening; a pager entry closes it.
func TestMenuTogglesStayOpen(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New()}
	m.layout(100, 30)

	m.openMenu()
	m.closeChoice(0) // "Send tools"
	if m.choice == nil {
		t.Fatal("menu closed after a toggle")
	}
	if got := m.choice.VisibleItems()[0].(choiceItem).text; got != "Send tools: off" {
		t.Fatalf("label = %q, want it to show the new state", got)
	}
	if m.choice.Index() != 0 {
		t.Fatalf("cursor moved to %d, want it to stay on the toggle", m.choice.Index())
	}

	m.closeChoice(menuEntry(t, m, "Show last context")) // opens a pager
	if m.choice != nil {
		t.Fatal("menu stayed open behind the pager")
	}
	if m.page == nil {
		t.Fatal("the entry did not open a pager")
	}
}

// Closing a pager or a submenu opened from the menu comes back to the menu,
// on the entry it was opened from, rather than dropping to the prompt.
func TestMenuReturnsFromPagerAndSubmenu(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New()}
	m.layout(100, 30)

	m.openMenu()
	context := menuEntry(t, m, "Show last context")
	m.closeChoice(context)
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc}); cmd != nil {
		t.Fatalf("esc on the pager returned a command: %v", cmd)
	}
	if m.page != nil {
		t.Fatal("esc did not close the pager")
	}
	if m.choice == nil {
		t.Fatal("menu did not come back after the pager closed")
	}
	if m.choice.Index() != context {
		t.Fatalf("cursor at %d, want it back on the entry that opened the pager", m.choice.Index())
	}

	m.closeChoice(menuEntry(t, m, "TTS language")) // opens a submenu
	if m.choice == nil {
		t.Fatal("the entry did not open a submenu")
	}
	m.closeChoice(-1) // cancel the submenu
	if m.choice == nil {
		t.Fatal("menu did not come back after the submenu was cancelled")
	}
	if got := m.choice.VisibleItems()[0].(choiceItem).text; !strings.HasPrefix(got, "Send tools") {
		t.Fatalf("came back to %q, want the main menu", got)
	}
}

// menuEntry finds an entry by the start of its label. The menu is indexed by
// position everywhere else, and a test that hard-codes those numbers breaks
// every time an entry is added above it.
func menuEntry(t *testing.T, m *tui, prefix string) int {
	t.Helper()
	for i, item := range m.choice.VisibleItems() {
		if strings.HasPrefix(item.(choiceItem).text, prefix) {
			return i
		}
	}
	t.Fatalf("no menu entry starting with %q", prefix)
	return -1
}

// ctrl+s flips the session from anywhere, so it works while the prompt has
// focus and a half-typed question in it.
func TestSessionHotkey(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New()}
	m.layout(100, 30)
	m.focus = paneInput
	m.setValue("half typed")

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if !m.sess.Stateless {
		t.Fatal("ctrl+s did not turn the session off")
	}
	if got := m.value(); got != "half typed" {
		t.Fatalf("the prompt lost its text: %q", got)
	}
	if !strings.Contains(ansi.Strip(m.logo), oblivium) {
		t.Fatalf("the logo does not carry the marker:\n%s", ansi.Strip(m.logo))
	}

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if m.sess.Stateless {
		t.Fatal("ctrl+s did not turn the session back on")
	}
	if strings.Contains(ansi.Strip(m.logo), oblivium) {
		t.Fatal("the marker outlived the session being turned back on")
	}
}

// ctrl+space opens the multiline box with the one-line draft carried over,
// the box grows as its content wraps past its floor, and sending shrinks it
// back down — the full round trip the height arithmetic exists for.
func TestMultilineToggleGrowsAndShrinks(t *testing.T) {
	// A client pointing nowhere: submit's send fails in the background, which
	// is all this test needs — the box's own height bookkeeping runs either way.
	client, err := llm.New(llm.Config{Provider: "openai", APIKey: "x", BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	m := &tui{ctx: context.Background(), sess: llm.NewSession(client, ""), oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New()}
	m.layout(30, 20)
	m.focus = paneInput
	m.setValue("draft")

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlAt}) // ctrl+space
	if !m.multiline {
		t.Fatal("ctrl+space did not open the multiline box")
	}
	if got := m.value(); got != "draft" {
		t.Fatalf("the draft was lost on toggle: %q", got)
	}
	if got := m.multiLine.Height(); got != minMultilineHeight {
		t.Fatalf("box opened at height %d, want the floor %d", got, minMultilineHeight)
	}

	// Enough text to wrap past the floor grows the box.
	long := strings.Repeat("a very long word ", 20)
	m.setValue(long)
	m.layout(m.width, m.height)
	if got := m.multiLine.Height(); got <= minMultilineHeight {
		t.Fatalf("box stayed at %d with content that overruns it", got)
	}

	// alt+enter sends in multiline mode and the box shrinks back to its floor.
	m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	if got := m.value(); got != "" {
		t.Fatalf("box still holds %q after sending", got)
	}
	if got := m.multiLine.Height(); got != minMultilineHeight {
		t.Fatalf("box rests at %d after sending, want the floor %d", got, minMultilineHeight)
	}
}

// Toggling a multi-line draft down to the one-row box must not lose the line
// breaks: textinput's own sanitizer silently turns a newline into a space,
// so the box folds them behind a visible marker instead, and toggling
// straight back up (untouched) must get the exact text back.
func TestMultilineToggleDoesNotLoseNewlines(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New()}
	m.layout(60, 20)
	m.focus = paneInput

	m.toggleMultiline()
	m.setValue("line one\nline two\nline three")
	m.toggleMultiline() // down to one-line

	if m.value() != "line one\nline two\nline three" {
		t.Fatalf("folded value() = %q, want the original text back", m.value())
	}
	if strings.Contains(m.oneLine.Value(), "\n") {
		t.Fatal("a real newline reached the one-row widget")
	}

	m.toggleMultiline() // back up, untouched since folding
	if got := m.multiLine.Value(); got != "line one\nline two\nline three" {
		t.Fatalf("multiline value after round trip = %q", got)
	}
}
