package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/charmbracelet/lipgloss"
	colorful "github.com/lucasb-eyer/go-colorful"
	rec_files "github.com/memmaker/rec-files"
)

// palette is every colour the TUI draws with, by the name colors.rec uses.
// These are the built-in defaults: the file only has to carry what it changes,
// and a missing file leaves the app fully coloured.
var palette = map[string]string{
	"border":     "240",     // pane borders
	"dim":        "240",     // muted text: notices, tool results, the logo field
	"accent":     "#ffb000", // sidebar labels, focused borders: P3 phosphor amber
	"error":      "161",
	"tool":       "70",      // tool call lines
	"focus":      "#ffb000", // focused scrollbar: the accent amber
	"anim_dark":  "#1a0000",
	"anim_light": "#ff1a1a",
	"logo_from":  "#ffb000", // logo ramp: amber at the top left…
	"logo_to":    "#ff1a1a",
	"on_accent":  "232", // …into the sweep's red at the bottom right
}

// LoadColors reads colors.rec — from the current directory, else from
// ~/.config/gollum — and rebuilds the styles from it. A missing file is not an
// error: the defaults stand. Unknown names are ignored, so a palette written
// for a newer version still loads.
func LoadColors() error {
	path, err := colorsPath()
	if err != nil || path == "" {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("error opening %s: %w", path, err)
	}
	defer f.Close()

	for _, r := range rec_files.Read(f) {
		m := r.ToMap(",")
		name, value := m["name"], m["value"]
		if name == "" || value == "" {
			continue
		}
		if _, known := palette[name]; !known {
			continue
		}
		palette[name] = value
	}
	applyColors()
	return nil
}

// colorsPath returns the colors file to read, or "" when there is none.
func colorsPath() (string, error) {
	if _, err := os.Stat("colors.rec"); err == nil {
		return filepath.Abs("colors.rec")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil // no home, no user palette: defaults are fine
	}
	p := filepath.Join(home, ".config", "gollum", "colors.rec")
	if _, err := os.Stat(p); err != nil {
		return "", nil
	}
	return p, nil
}

// applyColors rebuilds every style from the palette.
func applyColors() {
	borderStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(color("border"))
	focusedStyle = borderStyle.BorderForeground(color("accent"))
	dim = lipgloss.NewStyle().Foreground(color("dim"))
	label = lipgloss.NewStyle().Foreground(color("accent")).Bold(true)
	errStyle = lipgloss.NewStyle().Foreground(color("error"))
	// The OBLIVIUM marker on the logo: the error red, bold, because it says the
	// conversation is being thrown away after every turn.
	oblivumStyle = lipgloss.NewStyle().Foreground(color("error")).Bold(true)
	toolStyle = lipgloss.NewStyle().Foreground(color("tool"))
	focusStyle = lipgloss.NewStyle().Foreground(color("focus"))
	animDark = blendColor("anim_dark")
	animLight = blendColor("anim_light")
	accentStyle = lipgloss.NewStyle().Foreground(color("accent"))
	titleStyle = lipgloss.NewStyle().Foreground(color("on_accent")).Background(color("accent")).Padding(0, 1)
	logoFrom = blendColor("logo_from")
	logoTo = blendColor("logo_to")
}

func color(name string) lipgloss.Color { return lipgloss.Color(palette[name]) }

// blendColor parses the two colours the gradients interpolate between. They
// have to be RGB rather than terminal indexes: a palette index cannot be
// blended. An unparsable value falls back to black, which is visible as a
// mistake rather than a crash.
func blendColor(name string) colorful.Color {
	c, err := colorful.Hex(palette[name])
	if err != nil {
		return colorful.Color{}
	}
	return c
}
