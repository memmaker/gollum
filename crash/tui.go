package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/memmaker/gollum/llm"
	markterm "github.com/memmaker/gollum/mark-term"
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

	// wheelDelta is the rows one notch of the mouse wheel scrolls. The bubbles
	// default of 3 overshoots on a trackpad, which sends a notch per flick.
	wheelDelta = 1
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
	send, altEnter, newlineHint, multiline, oneLineHint, next, up, down, top, bottom, quit, pick, cancel, menu, history, session, render, interrupt key.Binding
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
	multiline:   key.NewBinding(key.WithKeys("ctrl+space"), key.WithHelp("ctrl+space", "multiline")),
	// Same key as multiline — matched nowhere, shown in its help line instead,
	// to name where it is about to send you back to.
	oneLineHint: key.NewBinding(key.WithKeys("ctrl+space"), key.WithHelp("ctrl+space", "one-line")),
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
	// Pager only: swap between rendered markdown and the raw source.
	render: key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "markdown")),
	// Never matched against a key — the input pane reads the arrows directly.
	// It carries them anyway because the help bar hides a binding with no keys,
	// and the point of this one is to say the history is there at all.
	history: key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑/↓", "history")),
	// Only live while a turn is running: cancels the request or tool chain in
	// flight, leaving the rest of the session untouched.
	interrupt: key.NewBinding(key.WithKeys("ctrl+x"), key.WithHelp("ctrl+x", "interrupt")),

	blockUp:   key.NewBinding(key.WithKeys("shift+up", "K"), key.WithHelp("shift+↑↓", "pick block")),
	blockDown: key.NewBinding(key.WithKeys("shift+down", "J"), key.WithHelp("shift+↓", "pick block")),
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

// askMsg is a tool asking the user to choose. reply carries the answer back —
// an offered option, or something the user typed instead — and is closed
// without a value if they cancelled.
type askMsg struct {
	question string
	options  []string
	reply    chan string
}

// askCancelMsg withdraws a question whose tool call has been cancelled. It
// names the reply channel rather than just saying "cancel": by the time it
// arrives the question may already have been answered, or replaced by a
// newer one, and neither should be torn down for it.
type askCancelMsg struct{ reply chan string }

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
	// follow sticks the output pane to the newest line. A scroll that lands on
	// the bottom turns it on, one that leaves the bottom turns it off, and
	// submitting a prompt turns it off so the new prompt stays at the top.
	follow bool

	current    string // provider/model in use
	transcript strings.Builder
	answer     strings.Builder
	reasoning  strings.Builder // provider's thought-summary text, streamed ahead of the answer
	stream     <-chan llm.Message
	stats      tokenStats // per-model token counts of every request this session
	lastPrompt string     // user text of the running turn, kept for the cost log
	legCalls   int        // tool calls seen since the running leg's last usage report
	legProse   bool       // whether the running leg produced any answer content
	legStart   time.Time  // first generated chunk of the running leg, zero before it arrives
	legEnd     time.Time  // its most recent generated chunk, for the leg's tokens-per-second
	// legChars is every character of model output streamed this leg and
	// legFirst the share of it that came in the very first chunk — the part
	// generated before legStart, and so outside the timed window. See
	// genWindow.
	legChars, legFirst int
	// callStart is when the request now in flight was sent, and callDurs how
	// long each of this turn's earlier requests took to come back. Zero
	// callStart means no request is waiting — the time between calls is tool
	// execution, which is not the backend's to answer for. See callTally.
	callStart time.Time
	callDurs  []time.Duration
	// turnCancel cancels the context handed to the running StreamWithTools
	// call, nil while idle. ctrl+x and an "inject now" pick both go through
	// this — the only lever we have on a turn already in flight.
	turnCancel context.CancelFunc
	// interrupted marks that the turn now winding down was cut short rather
	// than finishing on its own, so finishTurn can say so instead of guessing
	// from the absence of an error.
	interrupted bool
	// pendingInject is a prompt picked to jump the queue: set together with
	// turnCancel() so it fires the moment the cancelled turn's doneMsg lands.
	pendingInject string
	// queue holds prompts submitted while a turn was running and left to wait
	// their turn, oldest first.
	queue []string
	turnStart     int       // byte offset in the transcript where this turn's prompt echo starts
	turnStartedAt time.Time // wall clock start of the turn, for the ticking elapsed time in the statusbar
	note          string    // last error or status line, shown in the sidebar

	choice   *list.Model // non-nil while a choice overlay is up
	onChoice func(int)
	// choiceTitle is the overlay's question, wrapped and styled as its title
	// bar — the list draws no title of its own, so a long one keeps its tail.
	choiceTitle string

	logo        string    // fixed wordmark above the sidebar, "" when it does not fit
	page        *pager    // non-nil while the pager owns the screen
	sel         selection // line selection in one of the panes
	sideContent string    // sidebar text, unstyled by the selection

	blocks      []*toolBlock // every tool call (+ its result, once in) this session, in order
	blockCursor int          // index into blocks the output pane's block-nav is on; -1 = none

	// barVisible/barSeq drive the auto-hiding scrollbars, per pane: whether
	// the bar is up, and which hide timer is the live one.
	barVisible [3]bool
	barSeq     [3]int

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
	// side. Two `git` subprocesses to produce, so it is refreshed at the two
	// ends of a turn (see submit and finishTurn) rather than from
	// refreshSidebar, which runs on every scroll, drag and streamed chunk.
	// Empty outside a git repo.
	gitStatus string

	// contextSource names where the current model's context window came
	// from — the mapping file, a provider record, or models.dev — so
	// /context can say which number is in force and why. Empty when nobody
	// knew, which is also when the session's ContextLimit is zero.
	contextSource string

	// cwd is the working directory crash started in, shortened for display.
	// Fixed for the life of the process, so it's resolved once at startup
	// rather than on every View() call.
	cwd string

	// asking is the reply channel of the tool question currently outstanding,
	// whether its overlay is still up or the prompt has taken over answering
	// it. What lets a cancelled tool call withdraw its own question without
	// tearing down a newer one. See openAsk and cancelAsk.
	asking chan string

	// askReply is where the next line typed into the prompt goes: a tool asked
	// a question and the user chose to answer it in their own words rather
	// than off the list. Nil the rest of the time, which is when the prompt
	// means what it always means. See openAsk.
	askReply chan string

	// log is the append-only record of this session, in sessionFile. The
	// session writes its own messages there; this is the handle the harness
	// uses to record what it did around them — see sessionLog.note.
	log *sessionLog

	// pendingRestore is a conversation loaded from sessionFile, waiting to be
	// drawn into the transcript. Held here rather than rendered at startup
	// because rendering needs the pane width, which nothing knows until the
	// first WindowSizeMsg reaches layout — see replayRestoredHistory.
	pendingRestore []llm.Message
}

// ttsFailMsg reports that the engine died after it had started. It arrives on
// the event loop because the failure happens on the speech worker's goroutine.
type ttsFailMsg struct{ err error }

// runInteractive drives the four-pane TUI: output, sidebar, input, help.
func runInteractive(ctx context.Context, cfgs []llm.Config, model string, raw bool) {
	settings := LoadSettings()
	// Command-line -timeout flag takes precedence over the saved setting. This
	// has to land in cfgs[0].Timeout before the client is built: it is what
	// sizes the transport's own header-wait timeout, not just sess.Timeout.
	if cfgs[0].Timeout == llm.DefaultTimeout {
		cfgs[0].Timeout = settings.Timeout
	}
	client, err := llm.New(cfgs[0])
	if err != nil {
		die("error initializing client:", err)
	}
	sess := llm.NewSession(client, SystemPrompt())
	sess.Model = model
	sess.SendRegisteredTools = settings.SendTools
	sess.Stateless = !settings.Session
	sess.AutoCompact = settings.AutoCompact
	if settings.FullWidth {
		markterm.MaxWidth = 0
	}
	sess.Timeout = cfgs[0].Timeout
	sess.ServiceTier = cfgs[0].ServiceTier
	sess.ReasoningEffort = validEffort(cfgs[0].Provider, model, cfgs[0].ReasoningEffort)
	toolConfigs, _ := LoadToolConfigs()
	registerTools(sess, toolConfigs)

	// The conversation this directory's log already holds, from earlier runs
	// and earlier turns. Restored before the log is attached, or restoring it
	// would append it to the file it was just read from.
	restored := loadSession()
	sess.Restore(restored)
	sessLog := openSessionLog()
	sess.Log = sessLog.write

	// MCPs are optional: a config with none, or one that fails to load, still
	// starts crash — only the provider config in cfgs is load-bearing.
	mcpConfigs, _ := LoadMCPConfigs()
	mcpClients, mcpNotes := startMCPs(sess, mcpConfigs, toolConfigs)

	// Pricing is a bonus, not something worth stalling startup for: bounded
	// separately from ctx (which lives for the whole session) so a dead
	// network still starts crash on time, off a stale or missing cache.
	pricingCtx, cancelPricing := context.WithTimeout(ctx, 5*time.Second)
	prices := loadPricing(pricingCtx)
	cancelPricing()

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
		log:         sessLog,
		gitStatus:   gitStatusLine(),
		cwd:         cwdLine(),
		blockCursor: -1,
	}
	m.pendingRestore = restored
	m.stats.prices = prices
	// Needs the price table and m.current, so it cannot run before both are
	// on the model.
	m.refreshContextLimit()
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
	prog = tea.NewProgram(m)
	_, runErr := prog.Run()
	// Nothing to save on the way out: every message went into the log as it
	// happened. Closing it only reports a write that failed while the TUI had
	// the screen and no way to say so.
	if err := sessLog.close(); err != nil {
		fmt.Fprintln(os.Stderr, "could not write session log:", err)
	}
	if runErr != nil {
		die("tui:", runErr)
	}
}

func (m *tui) Init() tea.Cmd { return cursor.Blink }

// Scrollbars behave as crush's do: a bar is only ever there while its pane has
// something to scroll, the output pane's shows on a scroll and goes away
// barLinger later, and the sidebar's stands for as long as the sidebar has
// focus. Content arriving does not raise a bar — following the newest line is
// not scrolling anyone asked for.
const barLinger = 2 * time.Second

// barHideMsg drops a bar once its linger has run out. seq ignores the timers
// left behind by earlier scrolls, so the newest scroll always wins.
type barHideMsg struct {
	pane pane
	seq  int
}

// showBar raises a pane's scrollbar and schedules the hide that follows it.
func (m *tui) showBar(p pane) tea.Cmd {
	m.barVisible[p] = true
	m.barSeq[p]++
	seq := m.barSeq[p]
	return tea.Tick(barLinger, func(time.Time) tea.Msg { return barHideMsg{pane: p, seq: seq} })
}

// scrollPane is the pane a scroll acts on: the focused one, except from the
// prompt, which has nothing of its own to scroll and drives the output pane.
func (m *tui) scrollPane() pane {
	if m.focus == paneInput {
		return paneOutput
	}
	return m.focus
}

// barActive reports whether a pane's scrollbar belongs on screen.
func (m *tui) barActive(p pane) bool {
	vp := m.viewportFor(p)
	if vp.TotalLineCount() <= vp.Height() {
		return false // nothing to scroll, so nothing to say
	}
	if p == paneSidebar {
		// The sidebar only scrolls while it has focus, so focus is the whole
		// answer — and losing it takes the bar away in the same frame.
		return m.focus == paneSidebar
	}
	// The panes have no borders, so the bar is what says where focus is: it
	// stands for as long as the output pane holds it, and otherwise only for
	// the linger after a scroll driven from the prompt.
	return m.focus == paneOutput || m.barVisible[p]
}

func (m *tui) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case barHideMsg:
		if msg.seq == m.barSeq[msg.pane] {
			m.barVisible[msg.pane] = false
		}
		return m, nil

	case tea.WindowSizeMsg:
		m.layout(msg.Width, msg.Height)
		return m, nil

	case tea.ModeReportMsg:
		// The terminal's answer to Bubble Tea's mode 2027 query, which is what
		// switches its renderer from wcwidth to grapheme counting. We have to
		// land on the same answer it did — a frame measured one way and drawn
		// the other is the emoji drift — so this repeats its test exactly
		// rather than approximating it. In particular "permanently reset"
		// means the terminal will never do grapheme widths, which reads like an
		// answer about the mode but is a no: iTerm2 replies 4 here.
		if msg.Mode == ansi.ModeUnicodeCore && graphemeWidths(msg.Value) {
			markterm.WidthMethod = ansi.GraphemeWidth
			m.layout(m.width, m.height)
		}
		return m, nil

	case chunkMsg:
		m.chunk(llm.Message(msg))
		return m, waitChunk(m.stream)

	case doneMsg:
		m.finishTurn()
		return m, m.dispatchPending()

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
		m.openAsk(msg)
		return m, nil

	case askCancelMsg:
		m.cancelAsk(msg.reply)
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

	case tea.PasteMsg:
		// Only the prompt takes text; a paste anywhere else has nowhere to land.
		if m.page == nil && m.choice == nil && m.focus == paneInput {
			return m, m.paste(msg)
		}
		return m, nil

	case tea.KeyPressMsg:
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
		if key.Matches(msg, keys.interrupt) && m.stream != nil {
			m.interruptTurn()
			return m, nil
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
			m.redraw()
			return m, nil
		}
		if m.focus == paneInput {
			if msg.String() == "shift+up" {
				return m, m.scroll(tea.KeyPressMsg{Code: tea.KeyUp})
			}
			if msg.String() == "shift+down" {
				return m, m.scroll(tea.KeyPressMsg{Code: tea.KeyDown})
			}
			// pgup/pgdn read the transcript from the prompt, in either shape
			// of the box — the box itself walks with the arrows.
			if msg.Code == tea.KeyPgUp || msg.Code == tea.KeyPgDown {
				return m, m.scroll(msg)
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
				if msg.Code == tea.KeyUp {
					m.recall(-1)
					return m, nil
				}
				if msg.Code == tea.KeyDown {
					m.recall(1)
					return m, nil
				}
				var cmd tea.Cmd
				m.oneLine, cmd = m.oneLine.Update(msg)
				return m, cmd
			}
			var cmd tea.Cmd
			m.multiLine, cmd = m.multiLine.Update(msg)
			// The box grows and shrinks with its content as you type.
			m.layout(m.width, m.height)
			return m, cmd
		}
		if m.focus == paneOutput && m.handleBlockKey(msg) {
			// Block-nav scrolls the pane to the block it lands on, so it
			// raises the bar exactly as a scroll key does.
			return m, m.showBar(paneOutput)
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

// startCompress runs /compress as a turn of its own. It is a real round trip
// to the backend — on a long conversation, the slowest one there is — and
// before it was one it looked like the command had done nothing at all: no
// bar, no clock, no way to cancel it, and a prompt typed meanwhile went
// straight into a session that was about to be rewritten underneath it.
//
// Refused while another turn runs: compaction rewrites the history that turn
// is streaming against.
func (m *tui) startCompress() tea.Cmd {
	if m.stream != nil {
		m.note = "a turn is running"
		m.refreshSidebar()
		return nil
	}
	m.beginTurn("/compress")
	turnCtx, cancel := context.WithCancel(m.ctx)
	return m.startTurn(compressStream(turnCtx, m.sess), cancel)
}

// compressStream asks the model to summarize the conversation and replace the
// history with that summary, on the same channel a streamed answer arrives
// on, carrying the same Compacted message auto-compaction emits — so chunk
// and finishTurn need nothing of their own for it. A failure travels through
// sess.Err(), which finishTurn already reports for any turn.
func compressStream(ctx context.Context, sess *llm.Session) <-chan llm.Message {
	ch := make(chan llm.Message)
	go func() {
		defer close(ch)
		summary, err := sess.Compress(ctx)
		if err != nil {
			return
		}
		select {
		case ch <- llm.Message{Role: llm.RoleAssistant, Compacted: true, Content: summary}:
		case <-ctx.Done():
		}
	}()
	return ch
}

// setTimeout changes how long a backend may go silent before the turn is
// given up on, for the rest of the session — a local model that thinks for
// minutes before its first token needs more than the two-minute default. An
// empty arg reports the current value, and "0" turns the limit off entirely.
// The new value is persisted.
//
// Safe to write to the session directly: submit refuses every command while a
// stream is running, so the reader in nextChunk is never live at this point.
func (m *tui) setTimeout(arg string) tea.Cmd {
	switch d, err := time.ParseDuration(arg); {
	case arg == "":
		m.transcript.WriteString(dim.Render("timeout "+timeoutLabel(m.sess.Timeout)) + "\n\n")
	case err != nil || d < 0:
		m.note = "usage: /timeout 5m — a duration like 90s, 5m, 1h, or 0 for no limit"
		m.transcript.WriteString(dim.Render(m.note) + "\n\n")
	default:
		m.sess.SetTimeout(d)
		m.transcript.WriteString(dim.Render("timeout "+timeoutLabel(d)) + "\n\n")
		m.saveSettings()
	}
	m.setOutput(m.transcript.String())
	m.out.GotoBottom()
	m.refreshSidebar()
	return nil
}

// timeoutLabel names a timeout the way the session treats it: anything at or
// below zero disables the idle timer rather than expiring instantly.
func timeoutLabel(d time.Duration) string {
	if d <= 0 {
		return "off"
	}
	return d.String()
}

// refreshContextLimit re-resolves the context window for whatever model is
// current, and hands it to the session — which is what decides when a request
// is summarized instead of sent. Called at startup and on every model switch,
// since the window is a property of the model and not of the conversation.
func (m *tui) refreshContextLimit() {
	m.sess.ContextLimit, m.contextSource = resolveContextSize(m.current, m.cfgs, m.stats.prices)
}

// contextLine says which window is in force, where the number came from, and
// whether anything will act on it.
func (m *tui) contextLine() string {
	if m.sess.ContextLimit <= 0 {
		return "context window unknown for " + m.current + " — set it with /context 64000, until then nothing is compacted"
	}
	return fmt.Sprintf("context %s tokens (%s) · auto-compaction %s",
		commaInt(m.sess.ContextLimit), m.contextSource, onOff(m.sess.AutoCompact))
}

// setContextSize overrides the context window assumed for the current model
// and remembers it in the mapping file, which is the only one of the three
// sources that can be corrected from inside a running session — a gateway
// serving a model at a quarter of its published window is not something
// models.dev can be expected to know. An empty arg reports what is in force.
//
// Safe to write to the session directly, for the same reason setTimeout is:
// submit refuses every command while a stream is running.
func (m *tui) setContextSize(arg string) tea.Cmd {
	switch n, err := strconv.Atoi(strings.TrimSpace(arg)); {
	case arg == "":
		m.transcript.WriteString(dim.Render(m.contextLine()) + "\n\n")
	case err != nil || n <= 0:
		m.note = "usage: /context 64000 — the model's context window in tokens"
		m.transcript.WriteString(dim.Render(m.note) + "\n\n")
	default:
		m.sess.ContextLimit = n
		m.contextSource = contextMapFile
		if err := saveContextSize(m.current, n); err != nil {
			m.note = "could not save context size: " + err.Error()
		}
		m.transcript.WriteString(dim.Render(m.contextLine()) + "\n\n")
	}
	m.setOutput(m.transcript.String())
	m.out.GotoBottom()
	m.refreshSidebar()
	return nil
}

// submit acts on whatever is in the input box: a slash command, or a prompt.
// A command runs the moment it's typed, whether or not a turn is already
// running — a turn touches the session's own state (Clear, SetClient, the
// message history) that a command like /new or /model means to act on right
// then, not queued behind whatever the model is doing. Only a plain prompt
// gets the inject/queue choice, since two prompts in one conversation do need
// an order picked. See openSendMenu.
func (m *tui) submit() tea.Cmd {
	text := strings.TrimSpace(m.value())
	if text == "" {
		return nil
	}
	m.resetInput()
	m.layout(m.width, m.height) // the multiline box shrinks back to its floor
	m.remember(text)
	m.endRecall()

	// A tool is waiting on this line, not the model: it asked a question, the
	// offered answers did not fit, and this is the answer. Checked before the
	// running-turn guard below — the turn is running precisely because the
	// tool inside it is blocked on this.
	if m.askReply != nil {
		m.askReply <- text
		m.asking, m.askReply = nil, nil
		m.note = ""
		m.refreshSidebar()
		return nil
	}

	if m.stream != nil && !strings.HasPrefix(text, "/") {
		return m.openSendMenu(text)
	}
	return m.runText(text)
}

// openSendMenu asks whether a prompt typed while a turn is running should cut
// in or wait. Inject is first and so the overlay's default pick — the common
// case is "something changed, fold it into what's already running."
func (m *tui) openSendMenu(text string) tea.Cmd {
	m.openChoice("a turn is running", []string{
		"inject: send once the current call returns",
		"queue: send once the whole turn finishes",
	}, func(i int) {
		switch i {
		case 0:
			// Session.Inject splices the message into the next follow-up
			// request in the running tool chain, once the call now in flight
			// returns — it does not touch what's already running. A full
			// slot (another inject still unclaimed) falls back to queuing so
			// the prompt isn't dropped.
			if !m.sess.Inject(llm.Message{Role: llm.RoleUser, Content: text}) {
				m.queue = append(m.queue, text)
				break
			}
			m.pendingInject = text
		case 1:
			m.queue = append(m.queue, text)
		}
	})
	return nil
}

// interruptTurn cancels the request or tool chain now in flight — ctrl+x's
// only job. Safe to call with no turn running, in which case it does nothing.
func (m *tui) interruptTurn() {
	// A tool waiting on a typed answer holds the turn open with no overlay on
	// screen to escape from, so ctrl+x has to reach it too — otherwise the
	// only way out of a question the user does not want to answer is to kill
	// the program.
	// ponytail: the turn context alone would do it — user_choice waits on it
	// now — but only once the cancellation has travelled out to the tool's
	// goroutine and back. Closing here is what makes ctrl+x land in the same
	// frame it was pressed in; the later askCancelMsg finds nothing to do.
	if m.askReply != nil {
		m.asking = nil
		close(m.askReply)
		m.askReply = nil
		m.note = ""
	}
	if m.turnCancel == nil {
		return
	}
	m.interrupted = true
	m.turnCancel()
}

// dispatchPending starts whatever is next in line, once the current turn (if
// any) is actually done. An injected prompt only reaches here if the turn
// ended before ever sending the follow-up request it was meant for — the
// common path picks it up straight out of the running loop and this finds
// nothing left to drain. Otherwise the oldest queued prompt goes. Called
// after a turn ends and after any choice overlay closes, so a queue pick made
// after the turn it was waiting on already finished does not sit forgotten.
func (m *tui) dispatchPending() tea.Cmd {
	if m.stream != nil {
		return nil
	}
	if m.pendingInject != "" {
		m.pendingInject = ""
		if msg, ok := m.sess.DrainInject(); ok {
			return m.runText(msg.Content)
		}
		return nil // picked up inside the finished turn already
	}
	if len(m.queue) > 0 {
		text := m.queue[0]
		m.queue = m.queue[1:]
		return m.runText(text)
	}
	return nil
}

// runText sends one already-trimmed line to the session: a slash command, or
// a prompt to the model.
func (m *tui) runText(text string) tea.Cmd {
	// One command takes an argument, so it is matched on its first word
	// rather than on the whole line like the rest.
	if fields := strings.Fields(text); fields[0] == "/timeout" {
		return m.setTimeout(strings.Join(fields[1:], " "))
	}
	if fields := strings.Fields(text); fields[0] == "/context" {
		return m.setContextSize(strings.Join(fields[1:], " "))
	}

	switch text {
	case "/help":
		m.page = newMarkdownPager("commands", helpText+hotkeysText(), m.width, m.height)
		return nil
	case "/exit":
		return tea.Quit
	case "/model":
		return listModels(m.ctx, m.cfgs)
	case "/compress":
		return m.startCompress()
	case "/debug":
		m.debug = !m.debug
		m.transcript.WriteString(dim.Render("debug "+onOff(m.debug)) + "\n\n")
		m.setOutput(m.transcript.String())
		m.out.GotoBottom()
		m.refreshSidebar()
		return nil
	case "/new":
		m.newSession()
		return nil
	}

	m.beginTurn(text)

	// A turn's own cancellable context, so ctrl+x or an injected prompt can
	// stop this one request or tool chain without tearing down the session.
	turnCtx, cancel := context.WithCancel(m.ctx)
	ch, err := m.sess.StreamWithTools(turnCtx, text)
	if err != nil {
		cancel()
		m.note = err.Error()
		m.transcript.WriteString(dim.Render("error: "+err.Error()) + "\n\n")
		m.setOutput(m.transcript.String())
		m.refreshSidebar()
		return nil
	}
	return m.startTurn(ch, cancel)
}

// beginTurn opens a turn in the view: it echoes what started it and puts that
// echo at the top of the pane. Everything here is about what the user sees,
// so it runs for anything that occupies the session for a round trip — a
// prompt, or /compress, which is as much a call to the backend as a question
// is.
func (m *tui) beginTurn(text string) {
	// A new question means the last answer is history, including whatever is
	// still being read out of it.
	if m.speech != nil {
		m.speech.Stop()
	}

	m.note = ""
	m.lastPrompt = text
	m.turnStart = m.transcript.Len()
	m.turnStartedAt = time.Now()
	m.gitStatus = gitStatusLine()
	// Wrapped like every other line that reaches the pane. Written raw, a long
	// prompt ran past the pane edge and the viewport hard-truncated it, which
	// is also where an emoji-heavy prompt pushed the scrollbar out of line.
	m.transcript.WriteString(label.Render(markterm.Wrap("> "+text, max(m.out.Width(), 10))) + "\n\n")
	// Put the new prompt at the top of the pane: reading starts at the
	// question, not scrolled past it chasing the bottom. Whatever the last
	// turn was following ends here — scrolling down to the bottom again is
	// what asks for it back.
	m.follow = false
	m.setOutput(m.padForTurn(m.transcript.String()))
	m.out.SetYOffset(strings.Count(m.transcript.String()[:m.turnStart], "\n"))
}

// startTurn hands the view a running turn: ch is what waitChunk reads until
// it closes, and cancel is what ctrl+x pulls. Having a stream at all is what
// every "busy" answer in this file is made of — the animated bar, the ticking
// elapsed time, the interrupt binding, the send menu — so nothing that takes
// a round trip should run without going through here.
func (m *tui) startTurn(ch <-chan llm.Message, cancel context.CancelFunc) tea.Cmd {
	m.turnCancel = cancel
	m.stream = ch
	m.callStart = time.Now()
	m.callDurs = nil
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
		m.stats.addCall(m.current, m.lastPrompt, m.legCalls, m.legProse, *msg.Usage, m.sess.ServiceTier, m.genWindow(*msg.Usage))
		m.legCalls, m.legProse = 0, false
		m.legStart, m.legEnd = time.Time{}, time.Time{}
		m.legChars, m.legFirst = 0, 0
		m.refreshSidebar()
		return
	}
	if msg.Compacted {
		// Not answer content: the session summarized the conversation to keep
		// the next request inside the window. Drawn where it happened, so a
		// turn that suddenly stops referring to something says why.
		//
		// Still counts as produced: a /compress turn reports nothing else,
		// and a turn that says it compacted did not come back empty.
		m.produced = true
		m.flushReasoning()
		m.flushAnswer()
		m.endBlock()
		if msg.Err != nil {
			m.note = "compaction failed: " + msg.Err.Error()
			m.log.note("compaction failed: " + msg.Err.Error())
			m.transcript.WriteString(dim.Render("compaction failed: "+msg.Err.Error()) + "\n\n")
		} else {
			m.transcript.WriteString(dim.Render("compacted the conversation to fit the context window") + "\n\n")
		}
		m.setOutput(m.transcript.String())
		m.refreshSidebar()
		return
	}
	m.produced = true
	// Generation timing runs from the leg's first chunk to its last, which is
	// the rate the model actually produced text at. Tool results arrive on
	// this same channel, but the time a tool takes is not time spent
	// generating, so they leave the marks alone.
	if msg.Role != llm.RoleTool && msg.Role != llm.RoleUser {
		n := chunkChars(msg)
		if m.legStart.IsZero() {
			m.legStart = time.Now()
			m.legFirst = n
		}
		m.legChars += n
		m.legEnd = time.Now()
	}
	if msg.Reasoning != "" {
		if m.reasoning.Len() == 0 && m.answer.Len() == 0 {
			m.endBlock()
		}
		m.reasoning.WriteString(msg.Reasoning)
	}
	switch {
	case msg.Role == llm.RoleUser:
		// An injected prompt, spliced by the session into this running tool
		// chain's follow-up request — echoed the same way submit's own prompt
		// is, so it reads as part of the conversation rather than vanishing
		// into the model's next answer.
		m.flushReasoning()
		m.flushAnswer()
		m.transcript.WriteString(label.Render(markterm.Wrap("> "+msg.Content, max(m.out.Width(), 10))) + "\n\n")
	case len(msg.ToolCalls) > 0:
		// The model is done writing this leg; whatever comes next is the
		// tools running, then a fresh request.
		m.endCall()
		m.flushReasoning()
		m.flushAnswer()
		m.legCalls += len(msg.ToolCalls)
		for _, tc := range msg.ToolCalls {
			m.addToolCall(tc)
		}
	case msg.Role == llm.RoleTool:
		m.addToolResult(msg)
		// The follow-up request goes out once the last result is in, so each
		// result moves the mark forward and the last one leaves it right.
		m.callStart = time.Now()
	case msg.Content != "":
		if m.reasoning.Len() > 0 {
			m.flushReasoning()
		}
		if m.answer.Len() == 0 {
			// A fresh run of assistant text starts its own block: whatever came
			// before was a tool result or an earlier answer, and running into
			// it makes the two read as one sentence.
			m.endBlock()
		}
		m.legProse = true
		m.answer.WriteString(msg.Content)
		m.flushSettled()
	}
	live := m.transcript.String()
	if m.reasoning.Len() > 0 {
		live += m.renderReasoning(m.reasoning.String())
	}
	live += m.answer.String()
	m.setOutput(m.padForTurn(live))
}

// chunkChars is how much model output one streamed chunk carried: answer
// text, thought-summary text, and the tool call arguments it spelled out.
func chunkChars(msg llm.Message) int {
	n := len(msg.Content) + len(msg.Reasoning)
	for _, tc := range msg.ToolCalls {
		n += len(tc.Function.Name) + len(tc.Function.Arguments)
	}
	return n
}

// genWindow reports the leg's generation rate as the tokens that landed
// inside the timed window over the length of that window. The window opens on
// the first chunk, so the tokens that chunk carried were generated before it
// and do not belong to it — they are taken out by the share of the leg's
// characters that arrived afterwards, since the backend never says how many
// tokens any one chunk was worth.
//
// A leg that arrived in a single chunk therefore has no window at all, which
// is the honest answer: a tool call delivered as one aggregated message says
// nothing about how fast the model wrote it.
//
// ponytail: characters as the proxy for tokens. Exact within a leg would need
// per-chunk token counts no backend sends.
func (m *tui) genWindow(u llm.Usage) genWindow {
	if u.OutputTokens <= 0 || m.legChars <= m.legFirst {
		return genWindow{}
	}
	share := float64(m.legChars-m.legFirst) / float64(m.legChars)
	return genWindow{tokens: int(float64(u.OutputTokens) * share), d: m.legEnd.Sub(m.legStart)}
}

// padForTurn pads content with enough trailing blank lines that the current
// turn's prompt can reach the top of the pane: the viewport clamps
// SetYOffset to what the content can actually fill, and right after submit
// there's often not enough answer yet to push the prompt that high.
func (m *tui) padForTurn(s string) string {
	h := m.out.Height()
	if h <= 0 {
		return s
	}
	target := strings.Count(m.transcript.String()[:m.turnStart], "\n") + h
	have := strings.Count(s, "\n") + 1
	if have < target {
		s += strings.Repeat("\n", target-have)
	}
	return s
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
	m.transcript.WriteString(markterm.RenderMarkdown(settled, m.out.Width()) + "\n\n")
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

// renderReasoning dims a provider's thought-summary text under a "thinking"
// label, wrapped to the pane. Used both for the still-streaming tail (called
// fresh each chunk) and, via flushReasoning, for the finalized block.
func (m *tui) renderReasoning(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return dim.Render("◆ thinking") + "\n" + dim.Render(markterm.Wrap(s, m.out.Width())) + "\n\n"
}

// flushReasoning moves the still-open thought-summary block into the
// transcript once the answer proper starts, so it stops being redrawn every
// chunk and reads like the tool-call blocks around it.
func (m *tui) flushReasoning() {
	body := m.reasoning.String()
	m.reasoning.Reset()
	m.transcript.WriteString(m.renderReasoning(body))
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
		body = markterm.RenderMarkdown(body, m.out.Width())
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

// newSession starts a clean conversation: the output pane, the model's
// context, and the running-turn state are all reset. Prompt recall (up/down
// in the input box) is untouched — that history outlives any one context.
func (m *tui) newSession() {
	m.sess.Clear()
	m.transcript.Reset()
	m.answer.Reset()
	m.reasoning.Reset()
	m.blocks = nil
	m.blockCursor = -1
	m.note = ""
	m.lastPrompt = ""
	m.turnStart = 0
	m.setOutput(m.transcript.String())
	m.out.GotoTop()
	m.refreshSidebar()
}

// ellipsis shortens a single-line preview. Debug mode skips it.
//
// Cells, not bytes: len(s) counts UTF-8 bytes, so an emoji-heavy string
// compared true and then got sliced mid-rune by s[:width], corrupting the
// line and throwing off the pane's rendered width — the same class of bug
// fixed in markterm.visibleLen for the pager's scrollbar.
func ellipsis(s string, width int) string {
	s = strings.Join(strings.Fields(s), " ")
	if markterm.StringWidth(s) <= width {
		return s
	}
	return markterm.Truncate(s, "…", width)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// endCall closes the request in flight, if there is one, and files how long it
// took. Called where a leg's generation ends: at its tool calls, or at the end
// of the turn.
func (m *tui) endCall() {
	if m.callStart.IsZero() {
		return
	}
	m.callDurs = append(m.callDurs, time.Since(m.callStart))
	m.callStart = time.Time{}
}

// finishTurn re-renders the finished answer as markdown and reports how the
// stream ended. A backend can close a stream cleanly without sending anything,
// so an empty answer is worth saying out loud.
func (m *tui) finishTurn() {
	m.flushReasoning()
	m.flushAnswer()
	switch {
	case m.interrupted:
		// ctx cancellation unwinds StreamWithTools cleanly with no error of
		// its own, so this is the only place that knows the turn didn't just
		// finish on its own.
		m.transcript.WriteString(dim.Render("interrupted") + "\n\n")
		m.interrupted = false
	case m.sess.Err() != nil:
		err := m.sess.Err()
		m.note = err.Error()
		hint := ""
		if errors.Is(err, llm.ErrTimeout) {
			hint = " — try /model to switch provider"
		}
		m.transcript.WriteString(errStyle.Render("error: "+err.Error()+hint) + "\n\n")
	case !m.produced:
		m.transcript.WriteString(dim.Render("(no content returned)") + "\n\n")
	}
	m.endCall()
	if len(m.callDurs) > 0 {
		m.transcript.WriteString(dim.Render(callTally(m.callDurs)) + "\n\n")
	}
	m.answer.Reset()
	m.stream = nil
	m.turnCancel = nil
	// A turn that ran the bash tool may have changed the tree under us.
	m.gitStatus = gitStatusLine()
	// Still padded: trimming the pad here would shrink the content out from
	// under the current scroll position and yank the view. The pad only goes
	// away when the next turn recomputes it from scratch.
	m.setOutput(m.padForTurn(m.transcript.String()))
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
func (m *tui) updateChoice(msg tea.KeyPressMsg) tea.Cmd {
	filtering := m.choice.FilterState() == list.Filtering
	switch {
	case key.Matches(msg, keys.cancel) && !filtering:
		m.closeChoice(-1)
		return m.dispatchPending()
	case key.Matches(msg, keys.pick) && !filtering:
		idx := -1
		if it, ok := m.choice.SelectedItem().(choiceItem); ok {
			idx = it.idx
		}
		m.closeChoice(idx)
		return m.dispatchPending()
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
	if m.focus == paneSidebar && !m.sidebarFocusable() {
		m.focus = paneInput
	}
	if m.focus == paneInput {
		m.focusInput()
	} else {
		m.blurInput()
	}
	// The block-nav bar only makes sense while the output pane has focus;
	// redraw so it appears or vanishes with the change immediately.
	m.redraw()
}

// scroll routes a key to whichever pane has focus, and raises that pane's
// scrollbar for as long as it lingers. The prompt has nothing of its own to
// scroll, so the keys that reach here from it move the output pane.
func (m *tui) scroll(msg tea.KeyPressMsg) tea.Cmd {
	p := m.scrollPane()
	vp := m.viewportFor(p)
	var cmd tea.Cmd
	switch {
	case key.Matches(msg, keys.top):
		vp.GotoTop()
	case key.Matches(msg, keys.bottom):
		vp.GotoBottom()
	default:
		updated, c := vp.Update(msg)
		*vp = updated
		cmd = c
	}
	m.setFollow(p)
	return tea.Batch(cmd, m.showBar(p))
}

// setFollow re-reads whether the output pane is parked on its last line after
// a scroll. Reaching the bottom while an answer is streaming is the ask to
// ride along with it; scrolling back up is the ask to stop.
func (m *tui) setFollow(p pane) {
	if p == paneOutput {
		m.follow = m.out.AtBottom()
	}
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
	firstReady := !m.ready
	if !m.ready {
		m.out = viewport.New(viewport.WithWidth(outW), viewport.WithHeight(paneH))
		m.side = viewport.New(viewport.WithWidth(sideW), viewport.WithHeight(sideH))
		m.out.MouseWheelDelta = wheelDelta
		m.side.MouseWheelDelta = wheelDelta
		m.ready = true
	} else {
		m.out.SetWidth(outW)
		m.out.SetHeight(paneH)
		m.side.SetWidth(sideW)
		m.side.SetHeight(sideH)
	}
	if atBottom {
		m.out.GotoBottom()
	}
	if firstReady {
		m.replayRestoredHistory()
	}

	if m.page != nil {
		m.page.resize(w, h)
	}

	m.setInputWidth(w)
	if m.multiline {
		m.multiLine.SetHeight(inputH)
	}
	m.help.SetWidth(w)
	m.setOutput(m.transcript.String() + m.answer.String())
	m.refreshSidebar()
}

// redraw re-renders both panes, picking up a changed selection.
func (m *tui) redraw() {
	m.setOutput(m.transcript.String() + m.answer.String())
	m.refreshSidebar()
}

// setOutput re-wraps the transcript into the output pane. It only moves the
// scroll position while the pane is following the newest line; otherwise the
// view stays put and callers that want to jump it do so explicitly (submit's
// one-time scroll to the new prompt).
func (m *tui) setOutput(s string) {
	if !m.ready {
		return
	}
	m.out.SetContent(m.highlightBlock(m.sel.highlight(s, paneOutput)))
	if m.follow {
		m.out.GotoBottom()
	}
}

func (m *tui) refreshSidebar() {
	if !m.ready {
		return
	}
	var b strings.Builder
	heading := "model"
	if _, modelID, ok := lookupPrice(m.stats.prices, m.current); ok &&
		m.sess.ServiceTier == llm.ServiceTierFlex && flexEligible[modelID] {
		heading = "model (flex)"
	}
	fmt.Fprintf(&b, "%s\n%s\n", label.Render(heading), m.current)
	if len(m.reasoningChoices()) > 0 {
		fmt.Fprintf(&b, "Reasoning: %s\n", reasoningLabel(m.sess.ReasoningEffort))
	}
	b.WriteString("\n")
	if u := m.stats.total(); u.TotalTokens > 0 {
		// The per-model split and the reasoning share live in the "Request
		// costs" pager; the sidebar carries the few lines worth glancing at.
		fmt.Fprintf(&b, "%s\n%d in / %d out\n%d calls", label.Render("tokens"), u.InputTokens, u.OutputTokens, m.stats.calls)
		if cost := m.stats.totalCost(); cost > 0 {
			fmt.Fprintf(&b, " for $%.4f", cost)
		}
		if last, avg, ok := m.stats.rates(); ok {
			fmt.Fprintf(&b, "\n%.1f tok/s (avg %.1f)", last, avg)
		}
		// Against the window auto-compaction actually acts on, not against
		// models.dev's own number — those differ exactly when the user had
		// to correct one with /context, which is when the line matters.
		if limit := m.sess.ContextLimit; limit > 0 {
			if last, ok := m.stats.lastCall(m.current); ok {
				fmt.Fprintf(&b, "\n%s of %s ctx", commaInt(last.Usage.InputTokens), commaInt(limit))
			}
		}
		b.WriteString("\n\n")
	}
	if m.debug {
		fmt.Fprintf(&b, "%s\n%s\n\n", label.Render("debug"), "on")
	}
	if m.note != "" {
		fmt.Fprintf(&b, "\n%s\n%s\n", label.Render("last error"), m.note)
	}
	// Everything folds inside the pane rather than being cut: a model slug or
	// a path is worth reading to its end, and neither has spaces to break on,
	// so long words break mid-word.
	m.sideContent = markterm.Wrap(b.String(), m.side.Width())
	m.side.SetContent(m.sel.highlight(m.sideContent, paneSidebar))
	// The sidebar may have just shrunk out of the cycle from under the focus.
	if m.focus == paneSidebar && !m.sidebarFocusable() {
		m.focus = paneInput
		m.focusInput()
	}
}

// sidebarFocusable reports whether tab should stop at the sidebar: only when
// it is on screen at all and holds more than it can show. There is nothing to
// do in a pane that is entirely visible and has no cursor of its own, so it is
// left out of the cycle rather than made a stop that does nothing.
func (m *tui) sidebarFocusable() bool {
	return m.width >= 90 && m.side.TotalLineCount() > m.side.Height()
}

// graphemeWidths reports whether a DECRPM answer for mode 2027 means the
// terminal measures cells by grapheme cluster.
//
// Kept identical to the test Bubble Tea's event loop makes before switching
// its own renderer, because the two have to agree. Note what is missing:
// [ansi.ModePermanentlyReset] is an answer, but it is a no.
func graphemeWidths(v ansi.ModeSetting) bool {
	return v == ansi.ModeReset || v == ansi.ModeSet || v == ansi.ModePermanentlySet
}

func (m *tui) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "crash"
	if m.cwd != "" {
		v.WindowTitle += " " + m.cwd
	}
	return v
}

// render paints the whole screen and serialises it.
//
// The frame is composed as a grid of cells rather than by gluing strings side
// by side, because the two disagree about emoji. lipgloss.JoinHorizontal pads
// each line to a width it measures with ansi.StringWidth — hard-coded to
// grapheme counting, no way to tell it otherwise — so on a terminal that
// advances by wcwidth every line carrying an emoji comes out a cell short and
// drags the scrollbar and the sidebar left with it. Placing a pane at column
// N in a buffer cannot go wrong that way: N is an index, not the result of
// measuring what came before. A pane that mismeasures its own contents now
// distorts only itself, clipped at its own edge.
func (m *tui) render() string {
	if !m.ready {
		return "…"
	}
	canvas := uv.NewScreenBuffer(m.width, m.height)
	canvas.Method = markterm.WidthMethod
	m.paint(canvas)
	return canvas.Render()
}

// paint lays every region of the frame into its own rectangle of scr.
func (m *tui) paint(scr uv.ScreenBuffer) {
	if m.page != nil {
		m.page.paint(scr, m.help, uv.Rect(0, 0, m.width, m.height))
		return
	}

	y := 0
	paint(scr, animBar(m.width, m.frame, m.stream != nil), uv.Rect(0, y, m.width, animHeight))
	y += animHeight

	paneH := m.out.Height()
	paint(scr, m.out.View(), uv.Rect(0, y, m.out.Width(), paneH))
	paint(scr, scrollbar(m.out, m.barActive(paneOutput)), uv.Rect(m.out.Width(), y, 1, paneH))
	if m.width >= 90 {
		x, sideY := m.out.Width()+1, y
		// The logo is fixed above the sidebar rather than scrolling inside it.
		if m.logo != "" {
			logoH := lipgloss.Height(m.logo)
			paint(scr, m.logo, uv.Rect(x, sideY, m.side.Width(), logoH))
			sideY += logoH
		}
		paint(scr, m.side.View(), uv.Rect(x, sideY, m.side.Width(), m.side.Height()))
		paint(scr, scrollbar(m.side, m.barActive(paneSidebar)),
			uv.Rect(x+m.side.Width(), sideY, 1, m.side.Height()))
	}
	y += paneH

	input := m.inputView()
	inputH := lipgloss.Height(input)
	paint(scr, input, uv.Rect(0, y, m.width, inputH))
	y += inputH

	paint(scr, m.helpLine(), uv.Rect(0, y, m.width, helpHeight))

	// The overlay goes last so it covers whatever it lands on.
	if box := m.choiceView(); box != "" {
		w, h := boxSize(box)
		x, boxY := center(m.width, m.height, w, h)
		paint(scr, box, uv.Rect(x, boxY, w, h))
	}
}

// paint draws an already-rendered string into one rectangle of the screen,
// clipped to it.
func paint(scr uv.ScreenBuffer, s string, area uv.Rectangle) {
	uv.NewStyledString(s).Draw(scr, area)
}

// boxSize measures a rendered block the way the buffer will lay it out.
// lipgloss.Width would answer with grapheme counts even where the terminal
// advances by wcwidth, which puts a modal's right edge in the wrong column.
func boxSize(s string) (w, h int) {
	for _, line := range strings.Split(s, "\n") {
		w = max(w, markterm.StringWidth(line))
		h++
	}
	return w, h
}

// Scrollbar glyphs, as crush draws them.
const (
	scrollbarThumb = "┃"
	scrollbarTrack = "│"
)

// scrollbar draws a one-column track for a viewport, sized and positioned from
// how much of the content is on screen. It only shows while active — the bar
// has just moved or its pane has just taken focus; the rest of the time the
// column is blank, so the pane width never changes underneath it.
func scrollbar(vp viewport.Model, active bool) string {
	h := vp.Height()
	if h <= 0 {
		return ""
	}
	total := vp.TotalLineCount()
	if !active || total <= h {
		// Idle, or nothing to scroll: the bar says nothing worth a column of
		// glyphs. Its column stays, or the pane would jump when it comes back.
		return strings.TrimRight(strings.Repeat(" \n", h), "\n")
	}
	thumb := max(1, h*h/total)
	pos := int(float64(h-thumb)*vp.ScrollPercent() + 0.5)
	thumbStyle := focusStyle
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
			// The arrows scroll here as they do anywhere; the block cursor
			// rides along on them and steps on shift+arrows.
			b = []key.Binding{keys.up, keys.down, keys.blockUp, keys.fold, keys.next, keys.menu, keys.quit}
			break
		}
		fallthrough
	default:
		b = []key.Binding{keys.up, keys.down, keys.top, keys.bottom, keys.next, keys.menu, keys.quit}
	}
	if m.stream != nil {
		b = append([]key.Binding{keys.interrupt}, b...)
	}
	return m.padStatus(m.help.ShortHelpView(b))
}

// humanElapsed renders a duration the way the statusbar ticks it up while a
// turn runs: the coarsest two units that apply, seconds always present.
func humanElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	mn := d / time.Minute
	d -= mn * time.Minute
	s := d / time.Second
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm %ds", h, mn, s)
	case mn > 0:
		return fmt.Sprintf("%dm %ds", mn, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// contextStatus is the fraction of the model's context window in use, in the
// compact form the statusbar has room for — the sidebar spells out the same
// number in full. A real request's own reported usage is exact; a restored
// session that hasn't sent one yet this run only has an estimate, marked with
// "~" so it doesn't read as more certain than it is. Empty when the window is
// unknown, or nothing at all is in play yet.
func (m *tui) contextStatus() string {
	limit := m.sess.ContextLimit
	if limit <= 0 {
		return ""
	}
	if last, ok := m.stats.lastCall(m.current); ok {
		pct := float64(last.Usage.InputTokens) / float64(limit) * 100
		return dim.Render(fmt.Sprintf("%.0f%% ctx", pct))
	}
	if len(m.sess.History()) > 1 { // more than just the system prompt
		pct := float64(m.sess.EstimateTokens()) / float64(limit) * 100
		return dim.Render(fmt.Sprintf("~%.0f%% ctx", pct))
	}
	return ""
}

// padStatus appends the git statusbar to the right of the help line, padded
// out to the terminal width, with the working directory to its left when
// there's room for it too. Left alone when there is no room or no repo. While
// a turn is running, the elapsed wall time ticks up ahead of it — the tally
// callTally leaves in the transcript only totals the backend's own time, not
// the tool calls between requests, so this is the number that actually grows
// while the model is still thinking.
func (m *tui) padStatus(left string) string {
	right := m.gitStatus
	if ctx := m.contextStatus(); ctx != "" {
		if right == "" {
			right = ctx
		} else {
			right = ctx + "  " + right
		}
	}
	if m.stream != nil {
		elapsed := dim.Render(humanElapsed(time.Since(m.turnStartedAt)))
		if right == "" {
			right = elapsed
		} else {
			right = elapsed + "  " + right
		}
	}
	if right == "" {
		return left
	}
	if m.cwd != "" {
		withCwd := m.cwd + "  " + right
		if markterm.StringWidth(left)+markterm.StringWidth(withCwd) <= m.width {
			right = withCwd
		}
	}
	gap := m.width - markterm.StringWidth(left) - markterm.StringWidth(right)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}
