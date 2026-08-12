package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/memmaker/gollum/llm"

	markterm "github.com/memmaker/gollum/mark-term"
)

// The scrollbar is the only arithmetic in the view: a thumb that leaves the
// track, or a track of the wrong height, breaks the whole layout.
func TestScrollbar(t *testing.T) {
	vp := viewport.New(viewport.WithWidth(20), viewport.WithHeight(10))
	vp.SetContent(strings.Repeat("line\n", 5))
	if got := strings.Count(scrollbar(vp, true), "\n") + 1; got != 10 {
		t.Fatalf("short content: got %d rows, want 10", got)
	}
	// Content that fits has nothing to point at, so the column stays empty
	// however active the bar is.
	if got := strings.Count(scrollbar(vp, true), scrollbarThumb); got != 0 {
		t.Fatalf("short content drew %d thumb rows, want none", got)
	}

	vp.SetContent(strings.Repeat("line\n", 100))
	if scrollbar(vp, true) == scrollbar(vp, false) {
		t.Fatal("active and idle scrollbars look the same")
	}
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
	plain := m.render()

	m.openChoice("available models", []string{"openai/a", "openai/b"}, func(int) {})
	withModal := m.render()

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
	if m.render() != plain {
		t.Fatal("closing the overlay did not restore the plain view")
	}
}

// A tool asking a question in a sentence gets the whole sentence on screen:
// the title wraps over as many rows as it needs, and the overlay still fits
// the frame it is drawn into.
func TestChoiceWrapsLongTitle(t *testing.T) {
	m := &tui{
		sess:    llm.NewSession(nil, ""),
		cfgs:    []llm.Config{{Provider: "openai", Model: "gpt-5-mini"}},
		oneLine: newOneLineInput(), multiLine: newMultiLineInput(),
		help:    help.New(),
		current: "openai/gpt-5-mini",
	}
	m.layout(100, 30)
	plain := m.render()

	const question = "Which of these animals would you rather take along on a very long walk " +
		"through the woods, and why does it have to be that one in particular?"
	m.openChoice(question, []string{"cat", "dog", "owl"}, func(int) {})

	title := ansi.Strip(m.choiceTitle)
	if lipgloss.Height(title) < 2 {
		t.Fatalf("long title stayed on one row: %q", title)
	}
	if !strings.Contains(strings.Join(strings.Fields(title), " "), question) {
		t.Fatalf("title lost text:\n%s", title)
	}

	withModal := m.render()
	if lipgloss.Height(plain) != lipgloss.Height(withModal) || lipgloss.Width(plain) != lipgloss.Width(withModal) {
		t.Fatalf("wrapped title resized the frame: %dx%d then %dx%d",
			lipgloss.Width(plain), lipgloss.Height(plain),
			lipgloss.Width(withModal), lipgloss.Height(withModal))
	}
	if !strings.Contains(ansi.Strip(withModal), "in particular?") {
		t.Fatal("the end of the question never made it on screen")
	}
}

// Options can be sentences too: a row wraps over as many lines as it needs,
// and every row is given the same height so none slides over another.
func TestChoiceWrapsLongOptions(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), cfgs: []llm.Config{{Provider: "p"}}, oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New()}
	m.layout(100, 30)

	const long = "take the dog, because it will happily walk all day and never once ask how much further it is"
	m.openChoice("pick", []string{"cat", long}, func(int) {})

	view := ansi.Strip(m.choiceView())
	if !strings.Contains(view, "once ask how much further it is") {
		t.Fatalf("long option lost its tail:\n%s", view)
	}
	if strings.Contains(view, "…") {
		t.Fatalf("long option was truncated:\n%s", view)
	}
	rowH := choiceRowHeight([]string{long}, m.choice.Width())
	if rowH < 2 {
		t.Fatalf("delegate row height is %d, want room for the wrapped option", rowH)
	}
	// Two options at rowH rows each, and nothing overlapping: the box is at
	// least that tall.
	if got := lipgloss.Height(m.choiceView()); got < 2*rowH {
		t.Fatalf("overlay is %d rows, too short for two %d-row options", got, rowH)
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
	send := func(r rune) tea.Cmd { return m.updateChoice(tea.KeyPressMsg{Code: r, Text: string(r)}) }
	send('/')
	var cmd tea.Cmd
	for _, r := range "gem" {
		cmd = send(r)
	}
	m.run(t, cmd)
	if got := len(m.choice.VisibleItems()); got != 1 {
		t.Fatalf("filter matched %d items, want 1", got)
	}

	m.updateChoice(tea.KeyPressMsg{Code: tea.KeyEnter}) // apply the filter
	m.updateChoice(tea.KeyPressMsg{Code: tea.KeyEnter}) // choose the only match
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
	out = m.transcript.String()
	if !strings.Contains(out, "pick an animal") || !strings.Contains(out, `"cat"`) {
		t.Fatalf("debug mode still truncates the arguments:\n%s", out)
	}
	if !strings.Contains(out, "\n") {
		t.Fatal("debug mode should pretty-print arguments across multiple lines")
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
	}, []llm.Tool{
		{Function: llm.ToolFunction{Name: "user_choice", Description: "ask the user", Parameters: map[string]any{"type": "object"}}},
	})
	// The call's arguments are pretty-printed across lines, so check pieces of
	// the JSON rather than the exact compact string.
	for _, want := range []string{"pick one", "user_choice", `"options"`, `"cat"`, "cat", "tools offered", "ask the user", `"type": "object"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("context view is missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(renderContext(nil, nil), "no request") {
		t.Fatal("an empty context should say so")
	}
}

// The pager selects and copies with the mouse like the panes do.
func TestPagerMouseSelection(t *testing.T) {
	p := newPager("context", "alpha\nbeta\ngamma", 60, 20)

	press := tea.MouseClickMsg{X: 3, Y: titleHeight, Button: tea.MouseLeft}
	drag := tea.MouseMotionMsg{X: 3, Y: titleHeight + 1, Button: tea.MouseLeft}
	release := tea.MouseReleaseMsg{X: 3, Y: titleHeight + 1, Button: tea.MouseLeft}
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
	buf := uv.NewScreenBuffer(60, 20)
	buf.Method = markterm.WidthMethod
	p.paint(buf, help.New(), buf.Bounds())
	if !strings.Contains(buf.Render(), "copied") {
		t.Fatal("the pager does not confirm the copy on screen")
	}

	// A keypress clears the highlight, as in the panes.
	p.update(tea.KeyPressMsg{Code: tea.KeyDown})
	if p.sel.set {
		t.Fatal("a keypress left the selection up")
	}
}

// An idle bar shows nothing at all — scrollable or not — but keeps its column
// so nothing shifts when it comes back.
func TestScrollbarHiddenWhenIdle(t *testing.T) {
	vp := viewport.New(viewport.WithWidth(10), viewport.WithHeight(4))
	vp.SetContent("1\n2\n3\n4\n5\n6")

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
	if strings.TrimSpace(ansi.Strip(scrollbar(vp, true))) == "" {
		t.Fatal("active scrollbar is blank")
	}
	// Content that fits leaves the column empty even while active.
	vp.SetContent("one\ntwo")
	if strings.TrimSpace(ansi.Strip(scrollbar(vp, true))) != "" {
		t.Fatal("a pane that fits its content shows a bar")
	}
}

// The output bar comes up on a scroll and goes down when its own hide timer
// comes back; content arriving on its own raises nothing.
func TestScrollbarAutoHide(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New()}
	m.layout(100, 20)
	// From the prompt: the output bar is not a focus mark here, so it is only
	// ever up for the linger after a scroll.
	m.focus = paneInput
	m.setOutput(strings.Repeat("line\n", 50))
	if m.barActive(paneOutput) {
		t.Fatal("content arriving raised the output bar")
	}

	m.scroll(tea.KeyPressMsg{Code: tea.KeyUp})
	if !m.barActive(paneOutput) {
		t.Fatal("the output bar is down right after a scroll")
	}

	// A stale timer — one whose scroll has been superseded — leaves it up.
	m.Update(barHideMsg{pane: paneOutput, seq: m.barSeq[paneOutput] - 1})
	if !m.barActive(paneOutput) {
		t.Fatal("a stale hide timer took the bar down")
	}
	m.Update(barHideMsg{pane: paneOutput, seq: m.barSeq[paneOutput]})
	if m.barActive(paneOutput) {
		t.Fatal("the bar is still up after its own hide timer")
	}

	// Focus is the other way the bar comes up: it marks where focus is, so it
	// stands for as long as the output pane holds it.
	m.focus = paneOutput
	if !m.barActive(paneOutput) {
		t.Fatal("the output pane has focus and no bar to say so")
	}
	m.Update(barHideMsg{pane: paneOutput, seq: m.barSeq[paneOutput]})
	if !m.barActive(paneOutput) {
		t.Fatal("a hide timer took the focused pane's bar down")
	}

	// A pane with nothing to scroll never shows one, however fresh the scroll.
	m.setOutput("one line")
	m.scroll(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.barActive(paneOutput) {
		t.Fatal("a pane that fits its content shows a bar")
	}
}

// The sidebar's bar stands for exactly as long as the sidebar has focus.
func TestSidebarScrollbarFollowsFocus(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New()}
	m.layout(100, 20)
	m.sideContent = strings.Repeat("line\n", 50)
	m.side.SetContent(m.sideContent)

	if m.barActive(paneSidebar) {
		t.Fatal("the sidebar bar is up without focus")
	}
	m.focus = paneSidebar
	if !m.barActive(paneSidebar) {
		t.Fatal("the sidebar bar is down while the sidebar has focus")
	}
	m.focus = paneInput
	if m.barActive(paneSidebar) {
		t.Fatal("the sidebar bar outlived its focus")
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
	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc}); cmd != nil {
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

	m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	if !m.sess.Stateless {
		t.Fatal("ctrl+s did not turn the session off")
	}
	if got := m.value(); got != "half typed" {
		t.Fatalf("the prompt lost its text: %q", got)
	}
	if !strings.Contains(ansi.Strip(m.logo), oblivium) {
		t.Fatalf("the logo does not carry the marker:\n%s", ansi.Strip(m.logo))
	}

	m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
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

	m.Update(tea.KeyPressMsg{Code: tea.KeySpace, Mod: tea.ModCtrl}) // ctrl+space
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

	// ctrl+j sends in multiline mode and the box shrinks back to its floor —
	// this is the primary send key: alt+enter needs "Option as Meta" turned on
	// in most macOS terminals, off by default, so ctrl+j is the one guaranteed
	// to reach the app with no setup.
	m.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
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

// pgup/pgdn read the transcript without leaving the prompt, in either shape of
// the input box, and they raise the output pane's bar as any scroll does.
func TestPageKeysFromInput(t *testing.T) {
	for _, multiline := range []bool{false, true} {
		t.Run(fmt.Sprint("multiline=", multiline), func(t *testing.T) {
			m := &tui{sess: llm.NewSession(nil, ""), oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New()}
			m.layout(100, 20)
			m.focus = paneInput
			if multiline {
				m.toggleMultiline()
			}
			m.setOutput(strings.Repeat("line\n", 200))
			m.out.GotoBottom()
			bottom := m.out.YOffset()

			m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
			up := m.out.YOffset()
			if up >= bottom {
				t.Fatalf("pgup left the output pane at %d, want above %d", up, bottom)
			}
			if !m.barActive(paneOutput) {
				t.Fatal("scrolling from the prompt raised no scrollbar")
			}
			m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
			if got := m.out.YOffset(); got != bottom {
				t.Fatalf("pgdown left the output pane at %d, want back at %d", got, bottom)
			}
			if got := m.value(); got != "" {
				t.Fatalf("the page keys typed %q into the prompt", got)
			}
		})
	}
}

// Wide glyphs are two cells; counting bytes instead used to slice mid-rune
// and overrun the requested width, the same class of bug the pager's
// scrollbar had.
func TestEllipsisCountsWideGlyphsNotBytes(t *testing.T) {
	s := strings.Repeat("🚀", 20)
	got := ellipsis(s, 10)
	if w := ansi.StringWidth(got); w > 11 { // width + the "…" cell
		t.Fatalf("ellipsis %q is %d cells wide, want <= 11", got, w)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("ellipsis %q missing truncation marker", got)
	}
}

// The whole reason the frame is composed as cells rather than joined strings:
// emoji whose grapheme width and wcwidth disagree must not move any column.
// Under the old lipgloss.JoinHorizontal composition every transcript line
// carrying one of these came out short of the width lipgloss had measured, and
// the scrollbar and sidebar slid left along the whole row.
func TestEmojiKeepsColumnsPut(t *testing.T) {
	// U+26A0 + VS16 is one cell by wcwidth and two by grapheme clustering;
	// the skin-tone thumb is four against two. Both disagreements, one line.
	const emoji = "⚠️ warning 👍🏽 done"
	if ansi.WcWidth.StringWidth(emoji) == ansi.GraphemeWidth.StringWidth(emoji) {
		t.Fatal("test string no longer exercises a width disagreement")
	}

	for _, method := range []ansi.Method{ansi.WcWidth, ansi.GraphemeWidth} {
		t.Run(fmt.Sprint(method), func(t *testing.T) {
			defer func(prev ansi.Method) { markterm.WidthMethod = prev }(markterm.WidthMethod)
			markterm.WidthMethod = method

			m := &tui{
				sess:    llm.NewSession(nil, ""),
				cfgs:    []llm.Config{{Provider: "openai", Model: "gpt-5-mini"}},
				oneLine: newOneLineInput(), multiLine: newMultiLineInput(),
				help:    help.New(),
				current: "openai/gpt-5-mini",
			}
			const w, h = 100, 30
			m.layout(w, h)
			for range 40 {
				m.transcript.WriteString(emoji + "\n")
			}
			m.setOutput(m.transcript.String())
			m.barVisible[paneOutput] = true // the bar has to be on screen to be measured

			lines := strings.Split(m.render(), "\n")
			if len(lines) != h {
				t.Fatalf("frame is %d rows, want %d", len(lines), h)
			}
			for i, line := range lines {
				if got := method.StringWidth(line); got != w {
					t.Fatalf("row %d is %d cells, want %d: %q", i, got, w, ansi.Strip(line))
				}
			}

			// The scrollbar starts in exactly one column, immediately right of
			// the output pane, on every row the panes occupy — measured the way
			// the terminal measures, which is the column it actually lands in.
			col := m.out.Width()
			for row := animHeight; row < animHeight+m.out.Height(); row++ {
				plain := ansi.Strip(lines[row])
				at := strings.IndexAny(plain, scrollbarThumb+scrollbarTrack)
				if at < 0 {
					t.Fatalf("row %d has no scrollbar: %q", row, plain)
				}
				if got := method.StringWidth(plain[:at]); got != col {
					t.Fatalf("row %d put the scrollbar in column %d, want %d: %q", row, got, col, plain)
				}
			}
		})
	}
}

// The width method has to end up wherever Bubble Tea's renderer ends up: the
// frame is measured here and drawn there, and disagreement is the emoji drift.
// iTerm2 answers "permanently reset" to the mode 2027 query — an answer about
// the mode, but a no — and reading that as a yes puts the layout on grapheme
// widths while the renderer stays on wcwidth.
func TestGraphemeWidthsMatchesBubbleTea(t *testing.T) {
	for _, tc := range []struct {
		value ansi.ModeSetting
		want  bool
	}{
		{ansi.ModeNotRecognized, false},
		{ansi.ModeSet, true},
		{ansi.ModeReset, true},
		{ansi.ModePermanentlySet, true},
		{ansi.ModePermanentlyReset, false}, // iTerm2 3.6
	} {
		if got := graphemeWidths(tc.value); got != tc.want {
			t.Errorf("graphemeWidths(%v) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// ...and the model must actually apply that test to the message.
func TestModeReportKeepsWcWidthOnPermanentReset(t *testing.T) {
	defer func(prev ansi.Method) { markterm.WidthMethod = prev }(markterm.WidthMethod)
	markterm.WidthMethod = ansi.WcWidth

	m := &tui{
		sess:    llm.NewSession(nil, ""),
		cfgs:    []llm.Config{{Provider: "openai", Model: "gpt-5-mini"}},
		oneLine: newOneLineInput(), multiLine: newMultiLineInput(),
		help: help.New(),
	}
	m.layout(100, 30)
	m.Update(tea.ModeReportMsg{Mode: ansi.ModeUnicodeCore, Value: ansi.ModePermanentlyReset})
	if markterm.WidthMethod != ansi.WcWidth {
		t.Fatal("a permanently-reset answer switched the layout to grapheme widths")
	}
	m.Update(tea.ModeReportMsg{Mode: ansi.ModeUnicodeCore, Value: ansi.ModeSet})
	if markterm.WidthMethod != ansi.GraphemeWidth {
		t.Fatal("a set answer did not switch the layout to grapheme widths")
	}
}

// Tab skips the sidebar while it has nothing to scroll — a stop that can do
// nothing is a stop worth not making — and stops there once it overflows.
func TestTabSkipsSidebarWithNothingToScroll(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New()}
	m.layout(100, 30)
	m.focus = paneOutput

	m.cycleFocus()
	if m.focus != paneInput {
		t.Fatalf("tab stopped on pane %d, want the prompt: the sidebar fits", m.focus)
	}

	// Enough sidebar content to overflow, and it joins the cycle again.
	m.note = strings.Repeat("something went wrong. ", 200)
	m.refreshSidebar()
	m.focus = paneOutput
	m.cycleFocus()
	if m.focus != paneSidebar {
		t.Fatalf("tab stopped on pane %d, want the sidebar: it overflows", m.focus)
	}

	// A terminal too narrow to draw the sidebar takes it back out.
	m.layout(80, 30)
	m.focus = paneOutput
	m.cycleFocus()
	if m.focus != paneInput {
		t.Fatalf("tab stopped on pane %d, want the prompt: no sidebar at this width", m.focus)
	}
}

// The wheel raises the same bar the keys do, including from the prompt, where
// the wheel scrolls the output pane rather than anything of the prompt's own.
func TestWheelRaisesScrollbar(t *testing.T) {
	m := &tui{sess: llm.NewSession(nil, ""), oneLine: newOneLineInput(), multiLine: newMultiLineInput(), help: help.New()}
	m.layout(100, 20)
	m.focus = paneInput
	m.setOutput(strings.Repeat("line\n", 50))
	if m.barActive(paneOutput) {
		t.Fatal("the output bar is up before anything scrolled")
	}

	m.mouse(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if !m.barActive(paneOutput) {
		t.Fatal("the wheel scrolled the output pane without raising its bar")
	}
}
