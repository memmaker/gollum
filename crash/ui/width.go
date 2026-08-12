package ui

import "github.com/charmbracelet/x/ansi"

// WidthMethod is how many cells the terminal advances for a glyph, and every
// measurement the layout makes has to use it.
//
// There are two answers and they disagree on emoji. Legacy wcwidth reads
// ⚠️ (U+26A0 + VS16) as one cell and 👍🏽 as four; Unicode 9 grapheme clustering
// reads both as two. A terminal picks one, and gets asked which by DEC mode
// 2027: Bubble Tea queries it at startup and switches its own renderer to
// [ansi.GraphemeWidth] if the answer comes back yes. WcWidth is what the
// renderer starts at, so it is what we start at.
//
// Nothing in a string says which answer applies, which is why this is a
// variable set from the terminal's reply rather than a constant. Measuring
// with the other one is the whole emoji layout bug: pad a line to 80 by
// grapheme count on a terminal counting wcwidth and the line lands short,
// taking every column right of it along.
//
// [ansi.StringWidth] and friends are hard-coded to GraphemeWidth, so prefer
// the wrappers below over calling them directly.
var WidthMethod = ansi.WcWidth

// StringWidth is the cells s occupies, ANSI sequences skipped.
func StringWidth(s string) int { return WidthMethod.StringWidth(s) }

// Wrap breaks s to fit width cells, splitting long words mid-word — a model
// slug or a path has nowhere else to break.
func Wrap(s string, width int) string { return WidthMethod.Wrap(s, width, "") }

// Truncate cuts s down to width cells, tail included in the budget.
func Truncate(s, tail string, width int) string { return WidthMethod.Truncate(s, width, tail) }
