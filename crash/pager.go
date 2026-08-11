package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/memmaker/gollum/llm"
)

// pager is a full-screen scrollable reader for text too long for a pane. It
// owns the screen while it is up: esc closes it, everything else scrolls.
type pager struct {
	title   string
	content string // unstyled by the selection, so copying stays exact
	sel     selection
	note    string // copy confirmation, shown next to the help line
	vp      viewport.Model
	// onClose runs when the pager is dismissed. The menu uses it to come back
	// up, so closing a page opened from it returns there instead of the prompt.
	onClose func()
}

// titleHeight is the one row the pager spends on its title, above the body.
const titleHeight = 1

func newPager(title, body string, width, height int) *pager {
	// One row for the title, one for the help line at the bottom.
	vp := viewport.New(max(width-1, 10), max(height-titleHeight-1, 3))
	vp.SetContent(body)
	return &pager{title: title, content: body, vp: vp}
}

// mouse scrolls with the wheel and selects lines by dragging, exactly as the
// panes behind it do.
func (p *pager) mouse(msg tea.MouseMsg) {
	if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
		p.vp, _ = p.vp.Update(msg)
		return
	}
	row := min(max(msg.Y-titleHeight, 0), p.vp.Height-1)
	switch msg.Action {
	case tea.MouseActionPress:
		if msg.Button != tea.MouseButtonLeft || msg.Y < titleHeight {
			return
		}
		line := p.vp.YOffset + row
		p.sel = selection{pane: panePager, from: line, to: line, active: true, set: true}
	case tea.MouseActionMotion:
		if !p.sel.active {
			return
		}
		p.sel.to = p.vp.YOffset + row
	case tea.MouseActionRelease:
		if !p.sel.active {
			return
		}
		p.sel.active = false
		p.note = copyLines(p.content, p.sel.from, p.sel.to)
	}
	p.redraw()
}

func (p *pager) redraw() {
	p.vp.SetContent(p.sel.highlight(p.content, panePager))
}

func (p *pager) update(msg tea.KeyMsg) tea.Cmd {
	if p.sel.set {
		p.sel = selection{}
		p.redraw()
	}
	switch {
	case key.Matches(msg, keys.top):
		p.vp.GotoTop()
	case key.Matches(msg, keys.bottom):
		p.vp.GotoBottom()
	default:
		updated, cmd := p.vp.Update(msg)
		p.vp = updated
		return cmd
	}
	return nil
}

func (p *pager) view(h help.Model) string {
	note := ""
	if p.note != "" {
		note = "  " + p.note
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		label.Render(p.title),
		lipgloss.JoinHorizontal(lipgloss.Top, p.vp.View(), scrollbar(p.vp, true)),
		lipgloss.JoinHorizontal(lipgloss.Top,
			h.ShortHelpView([]key.Binding{keys.up, keys.down, keys.top, keys.bottom, keys.cancel}),
			dim.Render(note),
		),
	)
}

// renderContext lays out the messages of a request the way they were sent:
// role, then body, with tool calls and their results spelled out.
func renderContext(msgs []llm.Message) string {
	if len(msgs) == 0 {
		return dim.Render("no request has been sent yet")
	}
	var b strings.Builder
	for i, msg := range msgs {
		if i > 0 {
			b.WriteString("\n")
		}
		head := msg.Role
		if msg.Name != "" {
			head += " (" + msg.Name + ")"
		}
		fmt.Fprintf(&b, "%s\n", label.Render(head))
		if body := strings.TrimRight(msg.Content, "\n"); body != "" {
			fmt.Fprintf(&b, "%s\n", body)
		}
		for _, tc := range msg.ToolCalls {
			fmt.Fprintf(&b, "%s %s\n",
				toolStyle.Render("⚒ "+tc.Function.Name),
				dim.Render(strings.TrimSpace(tc.Function.Arguments)))
		}
	}
	return b.String()
}
