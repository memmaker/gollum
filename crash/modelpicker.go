package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/memmaker/crash/ui"
	"github.com/memmaker/gollum/llm"
)

// choiceItem is one line in the overlay list. It carries the caller's index,
// because filtering renumbers the rows the list actually shows.
type choiceItem struct {
	idx  int
	text string
}

func (c choiceItem) FilterValue() string { return c.text }

// choiceDelegate draws one row per item — the default delegate spends three.
type choiceDelegate struct{}

func (choiceDelegate) Height() int                         { return 1 }
func (choiceDelegate) Spacing() int                        { return 0 }
func (choiceDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }
func (choiceDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	line := "  " + item.(choiceItem).text
	if index == m.Index() {
		line = label.Render("▸ " + item.(choiceItem).text)
	}
	fmt.Fprint(w, ansi.Truncate(line, m.Width(), "…"))
}

// maxChoiceRows caps the overlay before it swallows the screen; past this the
// list paginates, which is what pagination is for.
const maxChoiceRows = 20

// modelEntry pairs a listed model with the provider config that serves it.
type modelEntry struct {
	cfg   llm.Config
	model string
}

type modelsMsg struct {
	entries []modelEntry
	errs    []string
}

// listModels asks every configured provider for its models. Network work, so
// it runs as a command rather than blocking the event loop.
func listModels(ctx context.Context, cfgs []llm.Config) tea.Cmd {
	return func() tea.Msg {
		var out modelsMsg
		for _, cfg := range cfgs {
			client, err := llm.New(cfg)
			if err != nil {
				out.errs = append(out.errs, cfg.Provider+": "+err.Error())
				continue
			}
			models, err := client.ListModels(ctx)
			if err != nil {
				out.errs = append(out.errs, cfg.Provider+": "+err.Error())
				continue
			}
			for _, m := range models {
				out.entries = append(out.entries, modelEntry{cfg: cfg, model: m})
			}
		}
		return out
	}
}

// openChoice puts a titled list overlay on screen and remembers what to do
// with the answer. pick is called with the chosen index, or -1 on cancel.
func (m *tui) openChoice(title string, options []string, pick func(int)) {
	items := make([]list.Item, len(options))
	for i, o := range options {
		items[i] = choiceItem{idx: i, text: o}
	}
	w := min(max(m.width*2/3, 30), m.width-4)
	rows := min(len(options), maxChoiceRows)
	maxH := max(m.height-4, 4)
	h := min(rows+1, maxH)

	l := list.New(items, choiceDelegate{}, w-2, h)
	l.Title = title
	// The list ships its own purple title bar; the palette owns that colour.
	l.Styles.Title = titleStyle
	// Same for the filter's green prompt and pink cursor. They have to be set
	// on the input as well: list.New copies the styles into it once, so a later
	// change to l.Styles alone never reaches the prompt.
	l.Styles.FilterPrompt = accentStyle
	l.Styles.FilterCursor = accentStyle
	l.FilterInput.PromptStyle = accentStyle
	l.FilterInput.Cursor.Style = accentStyle
	l.SetShowStatusBar(false)
	l.SetShowHelp(true)
	l.SetFilteringEnabled(true)
	l.Filter = substringFilter
	// The list subtracts its own title, pagination and help rows from the
	// height it is given, and how many those are depends on the content — a
	// fixed guess put five options on five pages. Grow until they fit.
	for l.Paginator.PerPage < rows && h < maxH {
		h++
		l.SetSize(w-2, h)
	}
	m.choice = &l
	m.onChoice = pick
}

// choiceView renders the overlay box, or "" when no choice is pending.
func (m *tui) choiceView() string {
	if m.choice == nil {
		return ""
	}
	return focusedStyle.Render(m.choice.View())
}

// applyModel switches the running session to the chosen provider and model,
// and remembers it as the default for the next run.
func (m *tui) applyModel(e modelEntry) {
	client, err := llm.New(e.cfg)
	if err != nil {
		m.note = err.Error()
		return
	}
	m.sess.SetClient(client)
	m.sess.Model = e.model
	m.current = e.cfg.Provider + "/" + e.model
	if err := SaveDefault(e.cfg.Provider, e.model); err != nil {
		m.note = "could not save default model: " + err.Error()
	}
}

// modalPos centres the overlay on screen.
func (m *tui) modalPos(box string) (int, int) {
	return center(m.width, m.height, lipgloss.Width(box), lipgloss.Height(box))
}

// substringFilter replaces the list's default fuzzy matcher. Fuzzy matching on
// model names is noise: "fable" subsequence-matches
// "omniroute/huggingface/black-forest-labs/FLUX.1-schnell". A plain
// case-insensitive substring is what a model name search actually means.
func substringFilter(term string, targets []string) []list.Rank {
	term = strings.ToLower(term)
	var out []list.Rank
	for i, t := range targets {
		at := strings.Index(strings.ToLower(t), term)
		if at < 0 {
			continue
		}
		matched := make([]int, len([]rune(term)))
		for j := range matched {
			matched[j] = at + j
		}
		out = append(out, list.Rank{Index: i, MatchedIndexes: matched})
	}
	return out
}

// saveSettings persists the menu toggles, reporting a failure in the sidebar
// rather than interrupting: a preference that did not stick is worth knowing
// about, but not worth stopping for.
func (m *tui) saveSettings() {
	if err := SaveSettings(Settings{
		SendTools:  m.sess.SendRegisteredTools,
		FullWidth:  ui.MaxWidth == 0,
		Session:    !m.sess.Stateless,
		TTSAnswers: m.ttsOn,
		TTSLang:    m.ttsLang,
		TTSEngines: m.ttsEngines,
	}); err != nil {
		m.note = "could not save settings: " + err.Error()
	}
}

// toggleSession switches whether the conversation is carried into the next
// request. Off, every prompt is asked cold — the model sees the system prompt
// and that prompt alone. The transcript keeps being recorded either way, so
// turning it back on resumes with everything that was said in between, which
// is why this is a toggle and not a way to lose a conversation.
func (m *tui) toggleSession() {
	m.sess.Stateless = !m.sess.Stateless
	m.saveSettings()
	// The logo carries the state: its tagline row gains OBLIVIUM.
	if m.logo != "" {
		m.logo = renderLogo("CRASH", m.side.Width, m.sess.Stateless)
	}
	m.transcript.WriteString(dim.Render("session "+onOff(!m.sess.Stateless)) + "\n\n")
	m.setOutput(m.transcript.String())
	m.out.GotoBottom()
	m.refreshSidebar()
}

// openMenu shows the main menu: entries that carry their own state render it,
// so the list doubles as the display.
func (m *tui) openMenu() {
	// stay keeps the menu up after the entry ran: a toggle is something you
	// flip and check, and reopening the menu by hand for that is a chore.
	entries := []struct {
		label string
		do    func()
		stay  bool
	}{
		{"Send tools: " + onOff(m.sess.SendRegisteredTools), func() {
			m.sess.SendRegisteredTools = !m.sess.SendRegisteredTools
			m.saveSettings()
			m.transcript.WriteString(dim.Render("send tools "+onOff(m.sess.SendRegisteredTools)) + "\n\n")
			m.setOutput(m.transcript.String())
			m.out.GotoBottom()
		}, true},
		{"Use full output width: " + onOff(ui.MaxWidth == 0), func() {
			if ui.MaxWidth == 0 {
				ui.MaxWidth = ui.DefaultMaxWidth
			} else {
				ui.MaxWidth = 0
			}
			m.saveSettings()
			m.transcript.WriteString(dim.Render("full output width "+onOff(ui.MaxWidth == 0)) + "\n\n")
			m.setOutput(m.transcript.String())
			m.out.GotoBottom()
		}, true},
		{"Session: " + onOff(!m.sess.Stateless), m.toggleSession, true},
		{"TTS for answers: " + onOff(m.ttsOn), func() {
			if !m.ttsOn {
				// Checked now, not at the first answer: a missing engine
				// should be an answer to the keypress that asked for it.
				if err := m.ttsUsable(); err != nil {
					m.note = "tts: " + err.Error()
					return
				}
			}
			m.ttsOn = !m.ttsOn
			if !m.ttsOn && m.speech != nil {
				m.speech.Stop()
			}
			m.saveSettings()
			m.transcript.WriteString(dim.Render("tts for answers "+onOff(m.ttsOn)) + "\n\n")
			m.setOutput(m.transcript.String())
			m.out.GotoBottom()
		}, true},
		{"TTS language: " + m.ttsLang, func() {
			langs := sortedKeys(m.ttsEngines)
			labels := make([]string, len(langs))
			for i, l := range langs {
				labels[i] = l + " — " + m.ttsEngines[l]
			}
			m.openChoice("tts language", labels, func(i int) {
				if i >= 0 {
					m.setTTSLang(langs[i])
				}
				m.refreshSidebar()
			})
		}, false},
		{"Show last context", func() {
			m.page = newPager("context sent with the last request",
				renderContext(m.sess.LastRequest()), m.width, m.height)
		}, false},
		{"Request costs", func() {
			m.page = newPager("token cost per request", m.stats.String(), m.width, m.height)
		}, false},
	}
	labels := make([]string, len(entries))
	for i, e := range entries {
		labels[i] = e.label
	}
	m.openChoice("menu", labels, func(i int) {
		if i >= 0 {
			// Reopen with the fresh labels, cursor back on the entry that was
			// just used.
			reopen := func() {
				m.openMenu()
				m.choice.Select(i)
			}
			entries[i].do()
			switch {
			case entries[i].stay:
				reopen()
			case m.page != nil:
				// The entry opened a pager: come back when it is dismissed.
				m.page.onClose = reopen
			case m.choice != nil:
				// The entry opened a submenu: come back after its own answer is
				// handled, cancel included.
				inner := m.onChoice
				m.onChoice = func(j int) {
					if inner != nil {
						inner(j)
					}
					reopen()
				}
			}
		}
		m.refreshSidebar()
	})
}
