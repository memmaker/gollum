package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/memmaker/crash/ui"
	"github.com/memmaker/gollum/llm"
)

// prog is the running TUI. Tools run on the session's goroutine and reach the
// event loop through it.
// ponytail: one program per process, which is all a CLI has.
var prog *tea.Program

const (
	// sidebarWidth is the logo plus its scrollbar column: the wordmark sets the
	// pane width exactly, no slack either side. Dropped entirely on a narrow
	// terminal.
	sidebarWidth = logoWidth + 1
	animHeight   = 1 // the bar across the top
	helpHeight   = 1
)

type pane int

const (
	paneInput pane = iota
	paneOutput
	paneSidebar

	// panePager is never focusable — focus cycles through the three panes
	// above. It exists to tag a selection as the pager's own.
	panePager
)

type keymap struct {
	send, altEnter, newlineHint, multiline, oneLineHint, next, up, down, top, bottom, quit, pick, cancel, menu, history, session key.Binding
	// blockUp/blockDown/fold are shown in the output pane's help line once it
	// holds a tool block — up/down and enter are already bound to selectBlock
	// and toggleSelectedBlock in handleBlockKey, these exist for their help text.
	blockUp, blockDown, fold key.Binding
}

var keys = keymap{
	// enter sends in the one-line prompt; in the multiline box it is left to
	// fall through to the textarea's own default of inserting a newline.
	send: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "send")),
	// ctrl+j (line feed, its own control byte — distinct from enter's CR) is
	// the multiline box's send key, the opposite of enter there so a box that
	// looks like an editor keeps enter meaning "newline". alt+enter works too,
	// where the terminal is configured to send it — macOS terminals need
	// "Option as Meta" turned on for that, off by default, so ctrl+j is the
	// one guaranteed to work without any setup.
	altEnter: key.NewBinding(key.WithKeys("ctrl+j", "alt+enter"), key.WithHelp("ctrl+j", "send")),
	// Never matched against a key — shown in the multiline help line only, to
	// say what plain enter does there since nothing else states it.
	newlineHint: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "newline")),
	multiline:   key.NewBinding(key.WithKeys("ctrl+@"), key.WithHelp("ctrl+space", "multiline")),
	// Same key as multiline — matched nowhere, shown in its help line instead,
	// to name where it is about to send you back to.
	oneLineHint: key.NewBinding(key.WithKeys("ctrl+@"), key.WithHelp("ctrl+space", "one-line")),
	next:        key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next pane")),
	up:          key.NewBinding(key.WithKeys("up", "k", "pgup"), key.WithHelp("↑/pgup", "scroll up")),
	down:        key.NewBinding(key.WithKeys("down", "j", "pgdown"), key.WithHelp("↓/pgdn", "scroll down")),
	top:         key.NewBinding(key.WithKeys("g", "home"), key.WithHelp("g", "top")),
	bottom:      key.NewBinding(key.WithKeys("G", "end"), key.WithHelp("G", "bottom")),
	quit:        key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
	pick:        key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "choose")),
	cancel:      key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
	// Not ctrl+m: terminals send CR for that, which is the same byte as enter.
	menu:    key.NewBinding(key.WithKeys("ctrl+p"), key.WithHelp("ctrl+p", "menu")),
	session: key.NewBinding(key.WithKeys("ctrl+s"), key.WithHelp("ctrl+s", "session")),
	// Never matched against a key — the input pane reads the arrows directly.
	// It carries them anyway because the help bar hides a binding with no keys,
	// and the point of this one is to say the history is there at all.
	history: key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑/↓", "history")),

	blockUp:   key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "pick block")),
	blockDown: key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "pick block")),
	fold:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "fold/expand")),
}

// Styles are built from the palette in colors.go; LoadColors rebuilds them
// once a colors.rec has been read.
var (
	borderStyle   lipgloss.Style
	focusedStyle  lipgloss.Style
	dim           lipgloss.Style
	label         lipgloss.Style
	errStyle      lipgloss.Style
	toolStyle     lipgloss.Style
	focusStyle    lipgloss.Style
	accentStyle   lipgloss.Style // accent without the bold of label
	oblivumStyle  lipgloss.Style // the OBLIVIUM marker: bold error red
	titleStyle    lipgloss.Style // overlay headings: text on an accent bar
	blockBarStyle lipgloss.Style // left bar marking the block-nav cursor's tool call
)

func init() { applyColors() }

type chunkMsg llm.Message
type doneMsg struct{}

// askMsg is a tool asking the user to choose. reply carries the index back, or
// -1 if the user cancelled.
type askMsg struct {
	question string
	options  []string
	reply    chan int
}

type tui struct {
	ctx  context.Context
	sess *llm.Session
	cfgs []llm.Config
	raw  bool

	out       viewport.Model
	side      viewport.Model
	oneLine   textinput.Model
	multiLine textarea.Model
	multiline bool // true while the multiline box (ctrl+space) is open
	// foldedDisplay/foldedOrig remember a multiline draft folded into the
	// one-row box (newlineFold in place of its line breaks) and the original
	// text with them intact, so toggling straight back up restores it exactly.
	foldedDisplay, foldedOrig string
	help                      help.Model

	width, height int
	focus         pane
	ready         bool

	current    string // provider/model in use
	transcript strings.Builder
	answer     strings.Builder
	stream     <-chan llm.Message
	stats      tokenStats // per-model token counts of every request this session
	lastPrompt string     // user text of the running turn, kept for the cost log
	turnStart  int        // byte offset in the transcript where this turn's prompt echo starts
	note       string     // last error or status line, shown in the sidebar

	choice   *list.Model // non-nil while a choice overlay is up
	onChoice func(int)

	logo        string    // fixed wordmark above the sidebar, "" when it does not fit
	page        *pager    // non-nil while the pager owns the screen
	sel         selection // line selection in one of the panes
	sideContent string    // sidebar text, unstyled by the selection

	blocks      []*toolBlock // every tool call (+ its result, once in) this session, in order
	blockCursor int          // index into blocks the output pane's block-nav is on; -1 = none

	frame    int  // animation step for the bar across the top
	debug    bool // /debug: show tool arguments and results in full
	produced bool // anything at all arrived this turn: text or tool activity

	history []string // prompts sent before, oldest first
	histAt  int      // where up/down has walked to; len(history) means "not walking"
	draft   string   // what was in the box when the walk started

	speech     *tts              // non-nil once an answer has actually been spoken
	ttsOn      bool              // menu toggle: read answers out loud
	ttsLang    string            // which configured engine speaks
	ttsEngines map[string]string // language key -> engine command

	mcpConfigs  []MCPConfig           // every `mcp:` record, on or off
	mcpClients  map[string]*mcpClient // running servers, keyed by config name
	toolConfigs []ToolConfig          // every built-in tool, on or off

	// gitStatus is the branch + diff tally shown on the statusbar's right
	// side, refreshed each turn rather than on every render — a subprocess
	// per animation frame would be wasteful. Empty outside a git repo.
	gitStatus string
}

// ttsFailMsg reports that the engine died after it had started. It arrives on
// the event loop because the failure happens on the speech worker's goroutine.
type ttsFailMsg struct{ err error }

// runInteractive drives the four-pane TUI: output, sidebar, input, help.
func runInteractive(ctx context.Context, cfgs []llm.Config, model string, raw bool) {
	client, err := llm.New(cfgs[0])
	if err != nil {
		die("error initializing client:", err)
	}
	sess := llm.NewSession(client, SystemPrompt())
	sess.Model = model
	settings := LoadSettings()
	sess.SendRegisteredTools = settings.SendTools
	sess.Stateless = !settings.Session
	if settings.FullWidth {
		ui.MaxWidth = 0
	}
	sess.Timeout = cfgs[0].Timeout
	toolConfigs, _ := LoadToolConfigs()
	registerTools(sess, toolConfigs)

	// MCPs are optional: a config with none, or one that fails to load, still
	// starts crash — only the provider config in cfgs is load-bearing.
	mcpConfigs, _ := LoadMCPConfigs()
	mcpClients, mcpNotes := startMCPs(sess, mcpConfigs, toolConfigs)

	oneLine := newOneLineInput()
	oneLine.Focus()

	m := &tui{
		ctx:         ctx,
		sess:        sess,
		cfgs:        cfgs,
		raw:         raw,
		oneLine:     oneLine,
		multiLine:   newMultiLineInput(),
		help:        help.New(),
		current:     cfgs[0].Provider + "/" + model,
		ttsOn:       settings.TTSAnswers,
		ttsLang:     settings.TTSLang,
		ttsEngines:  settings.TTSEngines,
		history:     loadHistory(),
		mcpConfigs:  mcpConfigs,
		mcpClients:  mcpClients,
		toolConfigs: toolConfigs,
		gitStatus:   gitStatusLine(),
		blockCursor: -1,
	}
	m.histAt = len(m.history)
	if len(mcpNotes) > 0 {
		m.note = strings.Join(mcpNotes, "; ")
	}
	// The engine outlives the program only if it is not killed here, and a
	// loaded model holds real memory. Every MCP subprocess is the same story.
	defer func() {
		if m.speech != nil {
			m.speech.Close()
		}
		for _, c := range m.mcpClients {
			c.close()
		}
	}()
	prog = tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := prog.Run(); err != nil {
		die("tui:", err)
	}
}

func (m *tui) Init() tea.Cmd { return cursor.Blink }

func (m *tui) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.layout(msg.Width, msg.Height)
		return m, nil

	case chunkMsg:
		m.chunk(llm.Message(msg))
		return m, waitChunk(m.stream)

	case doneMsg:
		m.finishTurn()
		return m, nil

	case mcpDeadMsg:
		if _, ok := m.mcpClients[msg.name]; ok {
			m.disableMCP(msg.name, "mcp: "+msg.name+" exited: "+msg.err.Error())
		}
		return m, nil

	case ttsFailMsg:
		// No respawn: the toggle is the retry. Close anyway — a write error
		// does not prove the engine process is gone.
		m.ttsOn = false
		if m.speech != nil {
			m.speech.Close()
			m.speech = nil
		}
		m.note = "tts: " + msg.err.Error()
		m.refreshSidebar()
		return m, nil

	case modelsMsg:
		if len(msg.errs) > 0 {
			m.note = strings.Join(msg.errs, "\n")
		}
		if len(msg.entries) == 0 {
			m.note = "no models available"
			m.refreshSidebar()
			return m, nil
		}
		labels := make([]string, len(msg.entries))
		for i, e := range msg.entries {
			labels[i] = e.cfg.Provider + "/" + e.model
		}
		m.openChoice("available models", labels, func(i int) {
			if i >= 0 {
				m.applyModel(msg.entries[i])
			}
			m.refreshSidebar()
		})
		return m, nil

	case askMsg:
		m.openChoice(msg.question, msg.options, func(i int) { msg.reply <- i })
		return m, nil

	case frameMsg:
		m.frame++
		if m.stream == nil {
			return m, nil // the bar rests until the next answer
		}
		return m, tick()

	case tea.MouseMsg:
		// The panes own the mouse: without this the terminal scrolls its own
		// scrollback out from under the TUI.
		if m.page != nil {
			m.page.mouse(msg)
			m.note = m.page.note
			return m, nil
		}
		if m.choice != nil {
			updated, cmd := m.choice.Update(msg)
			*m.choice = updated
			return m, cmd
		}
		return m, m.mouse(msg)

	case tea.KeyMsg:
		if m.page != nil {
			if key.Matches(msg, keys.cancel) || key.Matches(msg, keys.quit) {
				onClose := m.page.onClose
				m.page = nil
				if onClose != nil {
					onClose()
				}
				return m, nil
			}
			return m, m.page.update(msg)
		}
		if m.choice != nil {
			return m, m.updateChoice(msg)
		}
		m.clearSelection()
		if key.Matches(msg, keys.quit) {
			return m, tea.Quit
		}
		if key.Matches(msg, keys.menu) {
			m.openMenu()
			return m, nil
		}
		if key.Matches(msg, keys.session) {
			m.toggleSession()
			return m, nil
		}
		if key.Matches(msg, keys.next) {
			m.cycleFocus()
			return m, nil
		}
		if key.Matches(msg, keys.cancel) && m.focus != paneInput {
			m.focus = paneInput
			m.focusInput()
			return m, nil
		}
		if m.focus == paneInput {
			if msg.Type == tea.KeyShiftUp {
				return m, m.scroll(tea.KeyMsg{Type: tea.KeyUp})
			}
			if msg.Type == tea.KeyShiftDown {
				return m, m.scroll(tea.KeyMsg{Type: tea.KeyDown})
			}
			if key.Matches(msg, keys.multiline) {
				m.toggleMultiline()
				return m, nil
			}
			if !m.multiline && key.Matches(msg, keys.send) {
				return m, m.submit()
			}
			if m.multiline && key.Matches(msg, keys.altEnter) {
				return m, m.submit()
			}
			if !m.multiline {
				// A single row has no lines for the cursor to walk, so up and
				// down always recall — there is nothing else for them to do.
				if msg.Type == tea.KeyUp {
					m.recall(-1)
					return m, nil
				}
				if msg.Type == tea.KeyDown {
					m.recall(1)
					return m, nil
				}
				var cmd tea.Cmd
				m.oneLine, cmd = m.oneLine.Update(msg)
				return m, cmd
			}
			if msg.Type == tea.KeyPgUp {
				m.pageMultiline(true)
				return m, nil
			}
			if msg.Type == tea.KeyPgDown {
				m.pageMultiline(false)
				return m, nil
			}
			var cmd tea.Cmd
			m.multiLine, cmd = m.multiLine.Update(msg)
			// The box grows and shrinks with its content as you type.
			m.layout(m.width, m.height)
			return m, cmd
		}
		if m.focus == paneOutput && m.handleBlockKey(msg.String()) {
			return m, nil
		}
		return m, m.scroll(msg)
	}
	// The list filters through a command whose result comes back as its own
	// message: without this the overlay never learns what matched.
	if m.choice != nil {
		updated, cmd := m.choice.Update(msg)
		*m.choice = updated
		return m, cmd
	}
	return m, nil
}

// submit acts on whatever is in the input box: a slash command, or a prompt.
func (m *tui) submit() tea.Cmd {
	text := strings.TrimSpace(m.value())
	if text == "" || m.stream != nil {
		return nil
	}
	m.resetInput()
	m.layout(m.width, m.height) // the multiline box shrinks back to its floor
	m.remember(text)
	m.endRecall()

	switch text {
	case "/exit":
		return tea.Quit
	case "/model":
		return listModels(m.ctx, m.cfgs)
	case "/debug":
		m.debug = !m.debug
		m.transcript.WriteString(dim.Render("debug "+onOff(m.debug)) + "\n\n")
		m.setOutput(m.transcript.String())
		m.out.GotoBottom()
		m.refreshSidebar()
		return nil
	}

	// A new question means the last answer is history, including whatever is
	// still being read out of it.
	if m.speech != nil {
		m.speech.Stop()
	}

	m.note = ""
	m.lastPrompt = text
	m.turnStart = m.transcript.Len()
	m.transcript.WriteString(label.Render("> "+text) + "\n\n")
	m.setOutput(m.transcript.String())
	m.out.GotoBottom()

	ch, err := m.sess.StreamWithTools(m.ctx, text)
	if err != nil {
		m.note = err.Error()
		m.transcript.WriteString(dim.Render("error: "+err.Error()) + "\n\n")
		m.setOutput(m.transcript.String())
		m.refreshSidebar()
		return nil
	}
	m.stream = ch
	m.produced = false
	m.refreshSidebar()
	return tea.Batch(waitChunk(ch), tick())
}

// chunk folds one streamed message into the view. Content accumulates until
// the turn ends and can be rendered as markdown; tool activity is drawn as it
// happens, which means flushing the answer so far to keep the order honest.
func (m *tui) chunk(msg llm.Message) {
	if msg.Usage != nil {
		// Token counts, not an answer: nothing to draw in the transcript.
		m.stats.addCall(m.current, m.lastPrompt, *msg.Usage)
		m.refreshSidebar()
		return
	}
	m.produced = true
	switch {
	case len(msg.ToolCalls) > 0:
		m.flushAnswer()
		for _, tc := range msg.ToolCalls {
			m.addToolCall(tc)
		}
	case msg.Role == llm.RoleTool:
		m.addToolResult(msg)
	default:
		if m.answer.Len() == 0 {
			// A fresh run of assistant text starts its own block: whatever came
			// before was a tool result or an earlier answer, and running into
			// it makes the two read as one sentence.
			m.endBlock()
		}
		m.answer.WriteString(msg.Content)
		m.flushSettled()
	}
	m.setOutput(m.transcript.String() + m.answer.String())
	m.out.GotoBottom()
}

// flushSettled renders the part of the streaming answer that later chunks
// cannot change any more, and moves it into the transcript. Only the tail —
// the block still being written — stays raw, so the reader watches one
// paragraph settle instead of the whole answer reflowing when the turn ends.
func (m *tui) flushSettled() {
	if m.raw {
		return // nothing is reflowed in raw mode, so nothing has to settle
	}
	settled, rest := settledPrefix(m.answer.String())
	if settled == "" {
		return
	}
	m.answer.Reset()
	m.answer.WriteString(rest)
	m.speak(settled) // spoken from the markdown, before it grows ANSI
	m.transcript.WriteString(ui.RenderMarkdown(settled, m.out.Width) + "\n\n")
}

// settledPrefix splits a partial markdown answer into the part that is final
// and the part still being written. The cut is the last blank line that sits
// outside a code fence and is followed by a block that plainly starts fresh:
// a list, quote or table row after the blank could still be a continuation of
// what came before, and rendering the two halves apart would break it up.
//
// ponytail: line-level scan, not a parser. It cuts conservatively — worst case
// nothing settles early and the whole answer renders at the end, as it did
// before.
func settledPrefix(s string) (settled, rest string) {
	lines := strings.Split(s, "\n")
	fence := false
	cut := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fence = !fence
			continue
		}
		if fence || trimmed != "" || i == 0 {
			continue
		}
		if next := firstNonEmpty(lines[i+1:]); next == "" || continuesBlock(next) {
			continue
		}
		cut = i
	}
	if cut < 0 {
		return "", s
	}
	return strings.Join(lines[:cut], "\n"), strings.Join(lines[cut+1:], "\n")
}

func firstNonEmpty(lines []string) string {
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			return l
		}
	}
	return ""
}

// continuesBlock reports whether a line could belong to the construct above
// the blank line before it: a loose list item, a quote, a table row, or an
// indented continuation.
func continuesBlock(line string) bool {
	if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
		return true
	}
	trimmed := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(trimmed, "- "), strings.HasPrefix(trimmed, "* "), strings.HasPrefix(trimmed, "+ "):
		return true
	case strings.HasPrefix(trimmed, ">"), strings.HasPrefix(trimmed, "|"):
		return true
	}
	// "1. " and friends.
	digits := 0
	for digits < len(trimmed) && trimmed[digits] >= '0' && trimmed[digits] <= '9' {
		digits++
	}
	return digits > 0 && digits < len(trimmed) && (trimmed[digits] == '.' || trimmed[digits] == ')')
}

// flushAnswer moves whatever has streamed so far into the transcript, so a
// tool block does not jump ahead of the text that preceded it.
func (m *tui) flushAnswer() {
	body := m.answer.String()
	m.answer.Reset()
	if strings.TrimSpace(body) == "" {
		return
	}
	m.speak(body)
	if !m.raw {
		body = ui.RenderMarkdown(body, m.out.Width)
	}
	m.transcript.WriteString(body + "\n\n")
}

// endBlock leaves the transcript ending in exactly one blank line, so the next
// thing written starts a block of its own.
func (m *tui) endBlock() {
	body := strings.TrimRight(m.transcript.String(), "\n")
	if body == "" {
		return
	}
	m.transcript.Reset()
	m.transcript.WriteString(body + "\n\n")
}

// ellipsis shortens a single-line preview. Debug mode skips it.
//
// Cells, not bytes: len(s) counts UTF-8 bytes, so an emoji-heavy string
// compared true and then got sliced mid-rune by s[:width], corrupting the
// line and throwing off the pane's rendered width — the same class of bug
// fixed in ui.visibleLen for the pager's scrollbar.
func ellipsis(s string, width int) string {
	s = strings.Join(strings.Fields(s), " ")
	if ansi.StringWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// finishTurn re-renders the finished answer as markdown and reports how the
// stream ended. A backend can close a stream cleanly without sending anything,
// so an empty answer is worth saying out loud.
func (m *tui) finishTurn() {
	m.flushAnswer()
	if err := m.sess.Err(); err != nil {
		m.note = err.Error()
		hint := ""
		if errors.Is(err, llm.ErrTimeout) {
			hint = " — try /model to switch provider"
		}
		m.transcript.WriteString(errStyle.Render("error: "+err.Error()+hint) + "\n\n")
	} else if !m.produced {
		m.transcript.WriteString(dim.Render("(no content returned)") + "\n\n")
	}
	m.answer.Reset()
	m.stream = nil
	m.setOutput(m.transcript.String())
	// The markdown pass reflows the whole answer, so the view is wherever the
	// streaming left it. Put the prompt that started this turn at the top:
	// reading starts at the question, not at the last line of the reply. The
	// viewport clamps, so a short answer simply lands as high as it can.
	m.out.SetYOffset(strings.Count(m.transcript.String()[:m.turnStart], "\n"))
	m.refreshSidebar()
}

func waitChunk(ch <-chan llm.Message) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return doneMsg{}
		}
		return chunkMsg(msg)
	}
}

// updateChoice routes keys to the open overlay: enter answers, esc cancels,
// everything else scrolls or filters the list.
func (m *tui) updateChoice(msg tea.KeyMsg) tea.Cmd {
	filtering := m.choice.FilterState() == list.Filtering
	switch {
	case key.Matches(msg, keys.cancel) && !filtering:
		m.closeChoice(-1)
		return nil
	case key.Matches(msg, keys.pick) && !filtering:
		idx := -1
		if it, ok := m.choice.SelectedItem().(choiceItem); ok {
			idx = it.idx
		}
		m.closeChoice(idx)
		return nil
	}
	updated, cmd := m.choice.Update(msg)
	*m.choice = updated
	return cmd
}

// closeChoice tears the overlay down before handing the answer over, so the
// callback can open another one.
func (m *tui) closeChoice(idx int) {
	pick := m.onChoice
	m.choice, m.onChoice = nil, nil
	if pick != nil {
		pick(idx)
	}
}

func (m *tui) cycleFocus() {
	m.focus = (m.focus + 1) % 3
	if m.focus == paneInput {
		m.focusInput()
		return
	}
	m.blurInput()
}

// scroll routes a key to whichever pane has focus.
func (m *tui) scroll(msg tea.KeyMsg) tea.Cmd {
	vp := &m.out
	if m.focus == paneSidebar {
		vp = &m.side
	}
	switch {
	case key.Matches(msg, keys.top):
		vp.GotoTop()
	case key.Matches(msg, keys.bottom):
		vp.GotoBottom()
	default:
		updated, cmd := vp.Update(msg)
		*vp = updated
		return cmd
	}
	return nil
}

// layout splits the screen: output and sidebar side by side above the input
// box, with one help line at the very bottom.
func (m *tui) layout(w, h int) {
	m.width, m.height = w, h
	side := sidebarWidth
	if w < 90 {
		side = 0 // no room for two panes; the sidebar is the one to lose
	}

	// The one-line prompt spends a single row; the multiline box grows with
	// its content and spends two more on its own top and bottom border.
	inputH, border := oneLineHeight, 0
	if m.multiline {
		border = multilineBorderRows
		inputH = inputHeight(wrappedRows(m.value(), w-multilineBorderCols), h)
	}
	paneH := max(h-inputH-border-helpHeight-animHeight, 3)
	// Each pane spends one column on its scrollbar.
	outW := max(w-side-1, 10)
	sideW := max(side-1, 1)

	// The logo is not part of the scrolling sidebar: it sits above it and keeps
	// its rows, so the stats below can scroll under a fixed wordmark.
	m.logo = ""
	sideH := paneH
	if side > 0 && paneH > logoHeight+2 {
		m.logo = renderLogo("CRASH", sideW, m.sess.Stateless)
		sideH = paneH - lipgloss.Height(m.logo)
	}

	// A pane already scrolled to the newest line stays there through the
	// resize; one scrolled back to read something earlier is left alone.
	atBottom := m.ready && m.out.AtBottom()
	if !m.ready {
		m.out = viewport.New(outW, paneH)
		m.side = viewport.New(sideW, sideH)
		m.ready = true
	} else {
		m.out.Width, m.out.Height = outW, paneH
		m.side.Width, m.side.Height = sideW, sideH
	}
	if atBottom {
		m.out.GotoBottom()
	}

	m.setInputWidth(w)
	if m.multiline {
		m.multiLine.SetHeight(inputH)
	}
	m.help.Width = w
	m.setOutput(m.transcript.String() + m.answer.String())
	m.refreshSidebar()
}

// redraw re-renders both panes, picking up a changed selection.
func (m *tui) redraw() {
	m.setOutput(m.transcript.String() + m.answer.String())
	m.refreshSidebar()
}

// setOutput re-wraps the transcript into the output pane, keeping the scroll
// position unless the user was already at the bottom.
func (m *tui) setOutput(s string) {
	if !m.ready {
		return
	}
	atBottom := m.out.AtBottom()
	m.out.SetContent(m.highlightBlock(m.sel.highlight(s, paneOutput)))
	if atBottom {
		m.out.GotoBottom()
	}
}

func (m *tui) refreshSidebar() {
	if !m.ready {
		return
	}
	m.gitStatus = gitStatusLine()
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%s\n\n", label.Render("model"), m.current)
	if u := m.stats.total(); u.TotalTokens > 0 {
		// The per-model split and the reasoning share live in the "Request
		// costs" pager; the sidebar carries the one line worth glancing at.
		fmt.Fprintf(&b, "%s\n%d in / %d out in %d calls\n\n",
			label.Render("tokens"), u.InputTokens, u.OutputTokens, m.stats.calls)
	}
	if m.debug {
		fmt.Fprintf(&b, "%s\n%s\n\n", label.Render("debug"), "on")
	}
	if cwd, err := os.Getwd(); err == nil {
		fmt.Fprintf(&b, "%s\n%s\n\n", label.Render("cwd"), shortenHome(cwd))
	}
	if m.note != "" {
		fmt.Fprintf(&b, "\n%s\n%s\n", label.Render("last error"), m.note)
	}
	// Everything folds inside the pane rather than being cut: a model slug or
	// a path is worth reading to its end, and neither has spaces to break on,
	// so long words break mid-word.
	m.sideContent = ansi.Wrap(b.String(), m.side.Width, "")
	m.side.SetContent(m.sel.highlight(m.sideContent, paneSidebar))
}

func (m *tui) View() string {
	if !m.ready {
		return "…"
	}
	panes := renderPane(m.out, m.focus == paneOutput)
	if m.width >= 90 {
		side := renderPane(m.side, m.focus == paneSidebar)
		if m.logo != "" {
			side = lipgloss.JoinVertical(lipgloss.Left, m.logo, side)
		}
		panes = lipgloss.JoinHorizontal(lipgloss.Top, panes, side)
	}
	if m.page != nil {
		return m.page.view(m.help)
	}
	rows := []string{animBar(m.width, m.frame, m.stream != nil), panes}
	view := lipgloss.JoinVertical(lipgloss.Left,
		append(rows, m.inputView(), m.helpLine())...,
	)
	if box := m.choiceView(); box != "" {
		x, y := m.modalPos(box)
		view = overlay(view, box, x, y)
	}
	return view
}

// renderPane puts a viewport next to its scrollbar. With the borders gone,
// focus is shown by the scrollbar colour.
func renderPane(vp viewport.Model, focused bool) string {
	return lipgloss.JoinHorizontal(lipgloss.Top, vp.View(), scrollbar(vp, focused))
}

// Scrollbar glyphs, as crush draws them.
const (
	scrollbarThumb = "┃"
	scrollbarTrack = "│"
)

// scrollbar draws a one-column track for a viewport, sized and positioned from
// how much of the content is on screen. Content that fits gets a blank column,
// so the pane width never changes underneath it.
func scrollbar(vp viewport.Model, focused bool) string {
	h := vp.Height
	if h <= 0 {
		return ""
	}
	thumb, pos := h, 0
	total := vp.TotalLineCount()
	scrollable := total > h
	if scrollable {
		thumb = max(1, h*h/total)
		pos = int(float64(h-thumb)*vp.ScrollPercent() + 0.5)
	}
	if !scrollable && !focused {
		// Nothing to scroll and nothing to point at: the bar says neither, so
		// it goes away. Its column stays, or the pane would jump on focus.
		return strings.TrimRight(strings.Repeat(" \n", h), "\n")
	}
	// A pane whose content fits still shows a full-height thumb: the bar is
	// where focus is visible now that the panes have no borders.
	thumbStyle := dim
	if focused {
		thumbStyle = focusStyle
	}
	rows := make([]string, h)
	for i := range rows {
		if i >= pos && i < pos+thumb {
			rows[i] = thumbStyle.Render(scrollbarThumb)
		} else {
			rows[i] = dim.Render(scrollbarTrack)
		}
	}
	return strings.Join(rows, "\n")
}

// helpLine is context sensitive: it lists the keys that do something in the
// pane that currently has focus.
func (m *tui) helpLine() string {
	var b []key.Binding
	if m.choice != nil {
		return m.padStatus(m.help.ShortHelpView([]key.Binding{keys.pick, keys.up, keys.down, keys.cancel, keys.quit}))
	}
	switch m.focus {
	case paneInput:
		if m.multiline {
			b = []key.Binding{keys.altEnter, keys.newlineHint, keys.oneLineHint, keys.next, keys.menu, keys.quit}
		} else {
			b = []key.Binding{keys.send, keys.history, keys.multiline, keys.next, keys.menu, keys.quit}
		}
	case paneOutput:
		if len(m.blocks) > 0 {
			b = []key.Binding{keys.blockUp, keys.blockDown, keys.fold, keys.top, keys.bottom, keys.next, keys.menu, keys.quit}
			break
		}
		fallthrough
	default:
		b = []key.Binding{keys.up, keys.down, keys.top, keys.bottom, keys.next, keys.menu, keys.quit}
	}
	return m.padStatus(m.help.ShortHelpView(b))
}

// padStatus appends the git statusbar to the right of the help line, padded
// out to the terminal width. Left alone when there is no room or no repo.
func (m *tui) padStatus(left string) string {
	if m.gitStatus == "" {
		return left
	}
	gap := m.width - ansi.StringWidth(left) - ansi.StringWidth(m.gitStatus)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + m.gitStatus
}
