package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"

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
	fmt.Fprint(w, ui.Truncate(line, "…", m.Width()))
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
	// Same for the filter's green prompt and pink cursor. l.Styles.Filter is
	// only a template — the input list.New already built is what renders, so
	// the styles go straight on it.
	fs := l.FilterInput.Styles()
	fs.Focused.Prompt = accentStyle
	fs.Blurred.Prompt = accentStyle
	fs.Cursor.Color = color("accent")
	l.FilterInput.SetStyles(fs)
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
		m.logo = renderLogo("CRASH", m.side.Width(), m.sess.Stateless)
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
	// owns marks an entry whose do() fully manages the choice overlay from
	// here on, including finding its own way back to this menu — the generic
	// stay/page/choice dispatch below assumes a one-shot picker, which does
	// not fit a submenu that stays open across repeated toggles.
	entries := []struct {
		label string
		do    func()
		stay  bool
		owns  bool
	}{
		{"Send tools: " + onOff(m.sess.SendRegisteredTools), func() {
			m.sess.SendRegisteredTools = !m.sess.SendRegisteredTools
			m.saveSettings()
			m.transcript.WriteString(dim.Render("send tools "+onOff(m.sess.SendRegisteredTools)) + "\n\n")
			m.setOutput(m.transcript.String())
			m.out.GotoBottom()
		}, true, false},
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
		}, true, false},
		{"Session: " + onOff(!m.sess.Stateless), m.toggleSession, true, false},
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
		}, true, false},
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
		}, false, false},
		{"Show last context", func() {
			m.page = newPager("context sent with the last request",
				renderContext(m.sess.LastRequest(), m.sess.LastRequestTools()), m.width, m.height)
		}, false, false},
		{"Request costs", func() {
			m.page = newMarkdownPager("token cost per request", m.stats.String(), m.width, m.height)
		}, false, false},
		{"MCPs", func() { m.openMCPMenu() }, false, true},
		{"Tools", func() { m.openToolsMenu() }, false, true},
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
			case entries[i].owns:
				// do() already wired its own way back to this menu.
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

// openMCPMenu lists every configured MCP with its current on/off state.
// Unlike the model or TTS-language pickers, it stays open across a toggle
// instead of resolving to one answer — the same reasoning behind openMenu's
// "stay" entries — and returns to the main menu itself once cancelled.
func (m *tui) openMCPMenu() {
	labels := make([]string, len(m.mcpConfigs))
	for i, c := range m.mcpConfigs {
		labels[i] = c.Name + ": " + onOff(c.Enabled)
	}
	m.openChoice("MCPs", labels, func(i int) {
		if i < 0 {
			m.openMenu()
			m.refreshSidebar()
			return
		}
		m.toggleMCP(i)
		m.openMCPMenu()
		m.choice.Select(i)
		m.refreshSidebar()
	})
}

// toggleMCP flips one configured MCP on or off. Off kills its subprocess and
// drops its tools from the session; on spawns it fresh, the same path
// startup uses — a killed server leaves nothing worth resuming.
func (m *tui) toggleMCP(i int) {
	cfg := &m.mcpConfigs[i]
	cfg.Enabled = !cfg.Enabled
	if err := SaveMCPEnabled(cfg.Name, cfg.Enabled); err != nil {
		m.note = "mcp: could not save " + cfg.Name + ": " + err.Error()
	}

	if !cfg.Enabled {
		m.stopMCP(cfg.Name)
		return
	}

	c, err := spawnMCP(*cfg)
	if err != nil {
		cfg.Enabled = false
		m.note = "mcp: " + err.Error()
		return
	}
	m.mcpClients[cfg.Name] = c
	registerMCPTools(m.sess, c, m.toolConfigs)
}

// stopMCP kills a running client and drops its tools from the session. Safe
// to call for a name with no running client.
func (m *tui) stopMCP(name string) {
	c, ok := m.mcpClients[name]
	if !ok {
		return
	}
	for _, t := range c.tools {
		m.sess.RemoveTool(t.Name)
	}
	c.close()
	delete(m.mcpClients, name)
}

// disableMCP brings the menu and session back in step after an MCP's
// connection is found dead mid-session: its config is flipped off (in
// memory only — a transient crash should not permanently disable it in the
// saved config, matching how a startup spawn failure is handled) and its
// tools are dropped, same as toggling it off by hand.
func (m *tui) disableMCP(name, note string) {
	for i := range m.mcpConfigs {
		if m.mcpConfigs[i].Name == name {
			m.mcpConfigs[i].Enabled = false
			break
		}
	}
	m.stopMCP(name)
	m.note = note
	m.refreshSidebar()
}

// openToolsMenu lists every built-in tool plus every tool offered by a
// running MCP server, with its current on/off state. Like openMCPMenu, it
// stays open across toggles and returns to the main menu when cancelled.
func (m *tui) openToolsMenu() {
	names := m.toolMenuNames()
	labels := make([]string, len(names))
	for i, n := range names {
		labels[i] = n + ": " + onOff(toolEnabled(n, m.toolConfigs))
	}
	m.openChoice("Tools", labels, func(i int) {
		if i < 0 {
			m.openMenu()
			m.refreshSidebar()
			return
		}
		m.toggleTool(names[i])
		m.openToolsMenu()
		m.choice.Select(i)
		m.refreshSidebar()
	})
}

// toolMenuNames lists the built-ins plus every tool name currently offered
// by a live MCP client, so the Tools menu covers both without needing its
// own copy of what each MCP server advertised.
func (m *tui) toolMenuNames() []string {
	names := []string{"user_choice", "bash"}
	seen := map[string]bool{"user_choice": true, "bash": true}
	for _, c := range m.mcpClients {
		for _, t := range c.tools {
			if !seen[t.Name] {
				seen[t.Name] = true
				names = append(names, t.Name)
			}
		}
	}
	return names
}

// toggleTool flips one tool on or off by name — built-in or MCP-provided —
// and updates the session to match.
func (m *tui) toggleTool(name string) {
	enabled := !toolEnabled(name, m.toolConfigs)
	if err := SaveToolEnabled(name, enabled); err != nil {
		m.note = "tool: could not save " + name + ": " + err.Error()
		return
	}
	m.setToolConfig(name, enabled)

	m.sess.RemoveTool(name)
	registerTools(m.sess, m.toolConfigs)
	for _, c := range m.mcpClients {
		registerMCPTools(m.sess, c, m.toolConfigs)
	}

	m.transcript.WriteString(dim.Render("tool "+name+" "+onOff(enabled)) + "\n\n")
	m.setOutput(m.transcript.String())
	m.out.GotoBottom()
}

// setToolConfig records name's enabled state in m.toolConfigs in place,
// appending a new entry the first time an MCP tool is ever toggled.
func (m *tui) setToolConfig(name string, enabled bool) {
	for i := range m.toolConfigs {
		if m.toolConfigs[i].Name == name {
			m.toolConfigs[i].Enabled = enabled
			return
		}
	}
	m.toolConfigs = append(m.toolConfigs, ToolConfig{Name: name, Enabled: enabled})
}

// toolListing renders "Tools": every tool currently offered to the model,
// grouped by where it came from.
func (m *tui) toolListing() string {
	var b strings.Builder
	b.WriteString(label.Render("built-in") + "\n")
	for _, name := range []string{"user_choice", "bash"} {
		if toolEnabled(name, m.toolConfigs) {
			b.WriteString("  " + name + "\n")
		}
	}
	for _, cfg := range m.mcpConfigs {
		c, ok := m.mcpClients[cfg.Name]
		if !cfg.Enabled || !ok {
			continue
		}
		b.WriteString("\n" + label.Render(cfg.Name) + "\n")
		for _, t := range c.tools {
			if !toolEnabled(t.Name, m.toolConfigs) {
				continue
			}
			b.WriteString("  " + t.Name + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
