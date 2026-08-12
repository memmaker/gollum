package main

import (
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	colorful "github.com/lucasb-eyer/go-colorful"
)

// The bar across the top: a green ramp with a highlight travelling through it,
// after crush's animated header. Crush's own anim package is a character
// spinner built on bubbletea v2; this is the same idea at a tenth the size.
const (
	animFPS = 20
	// Upper one-eighth block: the ink sits at the top of its cell, so the bar
	// reads as a line under the window edge rather than under the first row.
	animChar  = "▔"
	animCycle = 60 // frames for the eye to cross the bar once
)

// A Cylon eye: near-black at rest, hot red at the centre of the sweep. Set
// from the palette — see colors.go.
var (
	animDark  colorful.Color
	animLight colorful.Color
	// The logo has its own ramp — see logo.go.
	logoFrom colorful.Color
	logoTo   colorful.Color
)

type frameMsg time.Time

// tick schedules the next animation frame.
func tick() tea.Cmd {
	return tea.Tick(time.Second/animFPS, func(t time.Time) tea.Msg { return frameMsg(t) })
}

// animBar renders one frame. The highlight is a raised cosine centred on a
// point that walks across the bar, so the colour fades in and out rather than
// stepping.
func animBar(width, frame int, active bool) string {
	if width <= 0 {
		return ""
	}
	if !active {
		// Blank, not dark: an idle bar should leave no mark at all, while
		// still holding its row so the panes below do not shift.
		return strings.Repeat(" ", width)
	}
	// Ping-pong: a triangle wave over two cycles, so the eye sweeps back the
	// way it came instead of jumping to the left edge.
	t := float64(frame%(2*animCycle)) / animCycle
	if t > 1 {
		t = 2 - t
	}
	head := t * float64(width-1)

	var b strings.Builder
	for i := range width {
		// Distance from the highlight, in cells, folded into a 0..1 weight.
		d := math.Abs(float64(i) - head)
		w := 0.0
		if span := float64(width) / 3; d < span {
			w = 0.5 * (1 + math.Cos(math.Pi*d/span))
		}
		b.WriteString(hue(animDark.BlendLuv(animLight, w)).Render(animChar))
	}
	return b.String()
}

// hue turns a blended colour into a lipgloss style.
func hue(c colorful.Color) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(c.Clamped().Hex()))
}
