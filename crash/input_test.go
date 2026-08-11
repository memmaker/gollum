package main

import "testing"

// The multiline box's height is the only non-trivial arithmetic the input
// mode adds: grow with content, floor at 3, and never crowd the animation
// bar, the output pane's own floor, the delimiter, or the help line off a
// short terminal.
func TestInputHeight(t *testing.T) {
	cases := []struct {
		name    string
		wrapped int
		term    int
		want    int
	}{
		{"empty content rests at the floor", 0, 40, 3},
		{"one line rests at the floor", 1, 40, 3},
		{"grows with content", 4, 40, 4},
		{"capped before it crowds the frame", 200, 40, 34}, // 40-1-3-1-1
		{"tiny terminal shrinks below the floor", 4, 8, 2}, // 8-1-3-1-1
		{"terminal too short even for one row floors at 1", 1, 4, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := inputHeight(c.wrapped, c.term); got != c.want {
				t.Fatalf("inputHeight(%d, %d) = %d, want %d", c.wrapped, c.term, got, c.want)
			}
		})
	}
}

func TestWrappedRows(t *testing.T) {
	cases := []struct {
		name  string
		s     string
		width int
		want  int
	}{
		{"empty string is still one row", "", 20, 1},
		{"short line is one row", "hello", 20, 1},
		{"exact width is one row", "0123456789", 10, 1},
		{"one over the width wraps to two", "01234567890", 10, 2},
		{"newlines count separately", "a\nb\nc", 20, 3},
		{"wide glyphs count cells, not runes", "🚀🚀🚀🚀🚀🚀", 10, 2}, // 12 cells / 10
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := wrappedRows(c.s, c.width); got != c.want {
				t.Fatalf("wrappedRows(%q, %d) = %d, want %d", c.s, c.width, got, c.want)
			}
		})
	}
}
