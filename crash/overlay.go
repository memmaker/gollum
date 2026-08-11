package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// overlay draws top over base at (x, y), keeping whatever base had to the left
// and right of it. Both sides are cut with ANSI-aware helpers, so a styled
// transcript underneath is not shredded into loose escape codes.
func overlay(base, top string, x, y int) string {
	lines := strings.Split(base, "\n")
	tops := strings.Split(top, "\n")
	width := lipgloss.Width(top)

	for i, tl := range tops {
		row := y + i
		if row < 0 || row >= len(lines) {
			continue
		}
		under := lines[row]
		left := ansi.Truncate(under, x, "")
		// A short line has to be padded out before the overlay can start at x.
		if pad := x - ansi.StringWidth(left); pad > 0 {
			left += strings.Repeat(" ", pad)
		}
		lines[row] = left + tl + ansi.TruncateLeft(under, x+width, "")
	}
	return strings.Join(lines, "\n")
}

// center returns the top-left corner that centres a w×h box in the screen.
func center(screenW, screenH, w, h int) (int, int) {
	return max((screenW-w)/2, 0), max((screenH-h)/2, 0)
}
