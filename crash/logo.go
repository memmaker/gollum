package main

import (
	"strings"

	colorful "github.com/lucasb-eyer/go-colorful"
)

// letterforms are the stylized glyphs the wordmark is drawn from: three rows
// of half blocks each, one column of air between letters. Only the letters of
// the app name exist, since nothing else is ever spelled.
var letterforms = map[rune][logoRows]string{
	'C': {"▄▀▀▀▀", "█    ", " ▀▀▀▀"},
	'R': {"█▀▀▀▄", "█▀▀▀▄", "▀   ▀"},
	'A': {"▄▀▀▀▄", "█▀▀▀█", "▀   ▀"},
	'S': {"▄▀▀▀▀", "▀▀▀▀█", "▀▀▀▀ "},
	'H': {"█   █", "█▀▀▀█", "▀   ▀"},
}

const (
	logoRows  = 3 // rows of the wordmark itself
	glyphCols = 5
	// logoWidth is what "CRASH" takes: five glyphs, one column apart.
	logoWidth = 5*glyphCols + 4
	// logoHeight is the wordmark plus the tagline and the diagonal fields
	// around it.
	logoHeight = logoRows + 4
	diag       = "╱"
	// tagline sits between the upper field and the wordmark, where Crush puts
	// its Charm™ line.
	tagline = "Override"
	// oblivium shares the tagline row when the session is off, pushed to the
	// right edge. Nothing is being remembered between turns, and that is worth
	// seeing without opening the menu.
	oblivium = "OBLIVIUM"
)

// renderLogo draws the wordmark the way Crush draws its own: diagonal fields
// above and below, the letters shaded on a diagonal ramp. Returns "" when the
// space is too narrow to hold it. forgetful marks a session that is not
// carrying its history into the next request.
func renderLogo(word string, width int, forgetful bool) string {
	if width < logoWidth {
		return ""
	}

	rows := make([]string, logoRows)
	for _, r := range word {
		glyph, ok := letterforms[r]
		if !ok {
			continue
		}
		for i := range rows {
			if rows[i] != "" {
				rows[i] += " "
			}
			rows[i] += glyph[i]
		}
	}

	out := make([]string, 0, logoHeight)
	field := accentStyle.Render(strings.Repeat(diag, logoWidth))
	// Padded to the full width so every row of the logo block is the same size.
	out = append(out, field, field, taglineRow(forgetful))
	for row, line := range rows {
		out = append(out, shadeRow(line, row))
	}
	out = append(out, field)
	return strings.Join(out, "\n")
}

// taglineRow is the line under the upper field: the tagline on the left, and
// on the right the warning that nothing is being remembered. Both are padded
// into exactly logoWidth columns, since every row of the block is that wide.
func taglineRow(forgetful bool) string {
	if !forgetful {
		return accentStyle.Render(tagline + strings.Repeat(" ", logoWidth-len(tagline)))
	}
	gap := logoWidth - len(tagline) - len(oblivium)
	if gap < 1 {
		gap = 1
	}
	return accentStyle.Render(tagline) + strings.Repeat(" ", gap) + oblivumStyle.Render(oblivium)
}

// shadeRow colours one row cell by cell. The ramp runs on the diagonal — the
// column and the row both push it — so the light crosses the word instead of
// running straight down it.
func shadeRow(line string, row int) string {
	var b strings.Builder
	for col, c := range []rune(line) {
		if c == ' ' {
			b.WriteString(" ")
			continue
		}
		t := (float64(col)/float64(logoWidth-1) + float64(row)/float64(logoRows-1)) / 2
		b.WriteString(hue(shade(t)).Render(string(c)))
	}
	return b.String()
}

// shade rides the logo's own ramp, which starts at the accent and lands on the
// colour the activity bar sweeps in.
func shade(t float64) colorful.Color {
	return logoFrom.BlendLuv(logoTo, t)
}
