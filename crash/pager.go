package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"

	markterm "github.com/memmaker/gollum/mark-term"
	"github.com/memmaker/gollum/llm"
)

// pager is a full-screen scrollable reader for text too long for a pane. It
// owns the screen while it is up: esc closes it, everything else scrolls.
type pager struct {
	title    string
	source   string // body as handed in, markdown still intact
	content  string // what is displayed and copied: source, or its rendered form
	markdown bool   // render source as markdown (ctrl+r toggles)
	sel      selection
	note     string // copy confirmation, shown next to the help line
	vp       viewport.Model
	// onClose runs when the pager is dismissed. The menu uses it to come back
	// up, so closing a page opened from it returns there instead of the prompt.
	onClose func()
}

// titleHeight is the one row the pager spends on its title, above the body.
const titleHeight = 1

func newPager(title, body string, width, height int) *pager {
	// One row for the title, one for the help line at the bottom.
	vp := viewport.New(viewport.WithWidth(max(width-1, 10)), viewport.WithHeight(max(height-titleHeight-1, 3)))
	vp.MouseWheelDelta = wheelDelta
	p := &pager{title: title, source: body, content: body, vp: vp}
	p.redraw()
	return p
}

// newMarkdownPager is newPager for a body written as markdown, opened with
// rendering already on.
func newMarkdownPager(title, body string, width, height int) *pager {
	p := newPager(title, body, width, height)
	p.markdown = true
	p.redraw()
	return p
}

// resize refits the pager to a new terminal size. Markdown is wrapped to the
// viewport width, so the body is re-rendered rather than just reflowed.
func (p *pager) resize(width, height int) {
	p.vp.SetWidth(max(width-1, 10))
	p.vp.SetHeight(max(height-titleHeight-1, 3))
	p.redraw()
}

// toggleMarkdown flips between the rendered view and the raw source. Raw is
// what you want to copy out or to read when the renderer mangles something,
// rendered is what you want for a table.
func (p *pager) toggleMarkdown() {
	p.markdown = !p.markdown
	p.sel = selection{}
	p.redraw()
}

// mouse scrolls with the wheel and selects lines by dragging, exactly as the
// panes behind it do.
func (p *pager) mouse(msg tea.MouseMsg) {
	m := msg.Mouse()
	row := min(max(m.Y-titleHeight, 0), p.vp.Height()-1)
	switch msg.(type) {
	case tea.MouseWheelMsg:
		p.vp, _ = p.vp.Update(msg)
		return
	case tea.MouseClickMsg:
		if m.Button != tea.MouseLeft || m.Y < titleHeight {
			return
		}
		line := p.vp.YOffset() + row
		p.sel = selection{pane: panePager, from: line, to: line, active: true, set: true}
	case tea.MouseMotionMsg:
		if !p.sel.active {
			return
		}
		p.sel.to = p.vp.YOffset() + row
	case tea.MouseReleaseMsg:
		if !p.sel.active {
			return
		}
		p.sel.active = false
		p.note = copyLines(p.content, p.sel.from, p.sel.to)
	}
	p.redraw()
}

func (p *pager) redraw() {
	p.content = p.source
	if p.markdown {
		p.content = markterm.RenderMarkdown(p.source, p.vp.Width())
	}
	p.vp.SetContent(p.sel.highlight(p.content, panePager))
}

func (p *pager) update(msg tea.KeyPressMsg) tea.Cmd {
	if p.sel.set {
		p.sel = selection{}
		p.redraw()
	}
	switch {
	case key.Matches(msg, keys.render):
		p.toggleMarkdown()
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

// paint lays the pager out in area: title row, body beside its scrollbar, help
// line. Same reason as the main frame — see tui.render — the columns are grid
// positions rather than the sum of measured strings, so an emoji in the body
// cannot walk the scrollbar out of its column.
func (p *pager) paint(scr uv.ScreenBuffer, h help.Model, area uv.Rectangle) {
	x, y, w := area.Min.X, area.Min.Y, area.Dx()
	uv.NewStyledString(label.Render(p.title)).Draw(scr, uv.Rect(x, y, w, titleHeight))
	y += titleHeight

	body := p.vp.Height()
	uv.NewStyledString(p.vp.View()).Draw(scr, uv.Rect(x, y, p.vp.Width(), body))
	uv.NewStyledString(scrollbar(p.vp, true)).Draw(scr, uv.Rect(x+p.vp.Width(), y, 1, body))
	y += body

	// The note keeps its columns at the right edge and the help line gives way
	// to it: help is long and reads fine cut short, a copy confirmation is short
	// and useless if it never appears. Both are clipped rather than allowed to
	// run past the pager's own width.
	help := h.ShortHelpView([]key.Binding{keys.up, keys.down, keys.top, keys.bottom, keys.render, keys.cancel})
	helpW := w
	if p.note != "" {
		noteW := markterm.StringWidth(p.note)
		helpW = max(w-noteW-2, 0)
		uv.NewStyledString(dim.Render(p.note)).Draw(scr, uv.Rect(x+w-noteW, y, noteW, 1))
	}
	uv.NewStyledString(help).Draw(scr, uv.Rect(x, y, helpW, 1))
}

// renderContext lays out the messages of a request the way they were sent:
// role, then body, with tool calls and their results spelled out, followed by
// the tool schema offered alongside them — what the model was told it could
// call, not just what it was told.
func renderContext(msgs []llm.Message, tools []llm.Tool) string {
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
			if msg.Role == "tool" {
				if pretty := prettyJSON(body); pretty != "" {
					body = pretty
				}
			}
			fmt.Fprintf(&b, "%s\n", body)
		}
		for _, tc := range msg.ToolCalls {
			fmt.Fprintf(&b, "%s\n", toolStyle.Render("⚒ "+tc.Function.Name))
			if pretty := prettyJSON(tc.Function.Arguments); pretty != "" {
				fmt.Fprintf(&b, "%s\n", dim.Render(pretty))
			}
		}
	}
	if len(tools) > 0 {
		fmt.Fprintf(&b, "\n%s\n", label.Render("tools offered"))
		for _, t := range tools {
			desc := strings.TrimSpace(t.Function.Description)
			params := ""
			if raw, err := json.Marshal(t.Function.Parameters); err == nil {
				params = prettyJSON(string(raw))
			}
			fmt.Fprintf(&b, "%s %s\n", toolStyle.Render("⚒ "+t.Function.Name), dim.Render(desc))
			if params != "" && params != "null" {
				fmt.Fprintf(&b, "%s\n", dim.Render(params))
			}
		}
	}
	return b.String()
}
