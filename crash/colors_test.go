package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadColors(t *testing.T) {
	shipped, err := os.ReadFile("../colors.rec") // the palette lives at the repo root
	if err != nil {
		t.Fatalf("read shipped palette: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "colors.rec"), shipped, 0o600); err != nil {
		t.Fatal(err)
	}
	// Append an override, so both the shipped values and a change are covered.
	f, err := os.OpenFile(filepath.Join(dir, "colors.rec"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("\nname: accent\nvalue: 99\n\nname: nonsense\nvalue: 1\n")
	f.Close()

	t.Chdir(dir)
	if err := LoadColors(); err != nil {
		t.Fatalf("LoadColors: %v", err)
	}
	if got := palette["accent"]; got != "99" {
		t.Fatalf("accent = %q, want 99 (override lost)", got)
	}
	if got := palette["tool"]; got != "70" {
		t.Fatalf("tool = %q, want 70 (shipped value lost)", got)
	}
	if _, ok := palette["nonsense"]; ok {
		t.Fatal("unknown name entered the palette")
	}
	// Gradient endpoints must survive as real RGB, or every blend goes black.
	if animLight.R < 0.9 || animDark.R == 0 {
		t.Fatalf("anim colours = %+v / %+v", animDark, animLight)
	}
	if got := label.GetForeground(); got != color("accent") {
		t.Fatalf("label foreground = %v, want the accent colour", got)
	}
}
