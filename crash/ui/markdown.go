package ui

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	gast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

// md parses with GFM on, so tables, strikethrough and task lists in model
// output are structure rather than stray punctuation.
var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

// ANSI escapes, kept together so the palette is one thing to change.
const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[90m"
	ansiItalic = "\033[3m"
	ansiUnder  = "\033[4m"
	ansiHead   = "\033[1;36m" // bold cyan
	ansiCode   = "\033[93m"   // yellow
	ansiStrike = "\033[9m"
)

// MaxWidth caps how wide rendered text is allowed to get, whatever the pane
// offers: long lines are hard to read. Zero lifts the cap.
// ponytail: a package variable, set once from the menu — one renderer per
// process, and it has no other state.
const DefaultMaxWidth = 100

var MaxWidth = DefaultMaxWidth

// RenderMarkdown turns markdown into ANSI-styled text for a terminal. Anything
// it cannot style is passed through as its own source text, so no content is
// ever lost to the renderer.
func RenderMarkdown(source string, width int) string {
	if MaxWidth > 0 && width > MaxWidth {
		width = MaxWidth
	}
	if width < 20 {
		width = 20
	}
	src := []byte(source)
	doc := md.Parser().Parse(text.NewReader(src))

	var out strings.Builder
	for node := doc.FirstChild(); node != nil; node = node.NextSibling() {
		block := renderBlock(node, src, width, "")
		if block == "" {
			continue
		}
		out.WriteString(block)
		if node.NextSibling() != nil {
			out.WriteString("\n")
		}
	}
	return strings.TrimRight(out.String(), "\n")
}

// renderBlock renders one block-level node, indenting every line it produces.
func renderBlock(node ast.Node, src []byte, width int, indent string) string {
	switch n := node.(type) {
	case *ast.Heading:
		text := ansiHead + strings.Repeat("#", n.Level) + " " + inline(n, src) + ansiReset
		return wrapIndent(text, width, indent) + "\n"

	case *ast.Paragraph, *ast.TextBlock:
		return wrapIndent(inline(node, src), width, indent) + "\n"

	case *ast.Blockquote:
		var out strings.Builder
		for child := n.FirstChild(); child != nil; child = child.NextSibling() {
			out.WriteString(renderBlock(child, src, width-2, indent+ansiDim+"│ "+ansiReset))
		}
		return out.String()

	case *ast.FencedCodeBlock, *ast.CodeBlock:
		var out strings.Builder
		lines := node.Lines()
		for i := 0; i < lines.Len(); i++ {
			seg := lines.At(i)
			out.WriteString(indent + "    " + ansiCode + strings.TrimRight(string(segValue(seg, src)), "\n") + ansiReset + "\n")
		}
		return out.String()

	case *ast.List:
		var out strings.Builder
		number := n.Start
		if number == 0 {
			number = 1
		}
		for item := n.FirstChild(); item != nil; item = item.NextSibling() {
			marker := "• "
			if n.IsOrdered() {
				marker = fmt.Sprintf("%d. ", number)
				number++
			}
			// The first line carries the marker, continuation lines align under
			// the text, which is what makes nested lists readable.
			body := ""
			for child := item.FirstChild(); child != nil; child = child.NextSibling() {
				body += renderBlock(child, src, width-len(marker), indent+strings.Repeat(" ", len(marker)))
			}
			body = strings.TrimLeft(body, " ")
			out.WriteString(indent + ansiDim + marker + ansiReset + body)
		}
		return out.String()

	case *ast.ThematicBreak:
		return indent + ansiDim + strings.Repeat("─", max(4, width-len(indent)-4)) + ansiReset + "\n"

	case *gast.Table:
		return renderTable(n, src, indent)

	default:
		// Anything goldmark hands back that has no styling here: print the
		// source lines as they were written.
		var out strings.Builder
		lines := node.Lines()
		for i := 0; i < lines.Len(); i++ {
			seg := lines.At(i)
			out.WriteString(indent + strings.TrimRight(string(segValue(seg, src)), "\n") + "\n")
		}
		if out.Len() == 0 {
			for child := node.FirstChild(); child != nil; child = child.NextSibling() {
				out.WriteString(renderBlock(child, src, width, indent))
			}
		}
		return out.String()
	}
}

// renderTable lays the cells out in columns. Cells joined end to end read as
// noise — comparing two rows means finding the same column twice, which the eye
// does by its edge, so every cell is padded to the widest in its column and the
// header gets a rule under it.
//
// The column's own alignment from the delimiter row is honoured; markdown's
// `---:` exists because a column of numbers wants to line up on the right.
func renderTable(n *gast.Table, src []byte, indent string) string {
	var rows [][]string
	var isHeader []bool
	for row := n.FirstChild(); row != nil; row = row.NextSibling() {
		var cells []string
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			cells = append(cells, strings.TrimSpace(inline(cell, src)))
		}
		rows = append(rows, cells)
		_, header := row.(*gast.TableHeader)
		isHeader = append(isHeader, header)
	}

	// A malformed row can carry more cells than the delimiter row declared
	// columns, so the widths are measured, not assumed.
	var widths []int
	for _, cells := range rows {
		for i, c := range cells {
			for len(widths) <= i {
				widths = append(widths, 0)
			}
			if w := visibleLen(c); w > widths[i] {
				widths[i] = w
			}
		}
	}

	sep := ansiDim + " │ " + ansiReset
	var out strings.Builder
	for r, cells := range rows {
		style := ""
		if isHeader[r] {
			style = ansiBold
		}
		padded := make([]string, len(cells))
		for i, c := range cells {
			align := columnAlign(n, i)
			// The last column is left as it is when it aligns left: padding it
			// would only trail invisible spaces to the end of the line.
			if i == len(cells)-1 && (align == gast.AlignLeft || align == gast.AlignNone) {
				padded[i] = style + c + ansiReset
				continue
			}
			padded[i] = style + padCell(c, widths[i], align) + ansiReset
		}
		out.WriteString(indent + strings.Join(padded, sep) + "\n")
		if isHeader[r] {
			rule := make([]string, len(widths))
			for i, w := range widths {
				rule[i] = strings.Repeat("─", w)
			}
			out.WriteString(indent + ansiDim + strings.Join(rule, "─┼─") + ansiReset + "\n")
		}
	}
	return out.String()
}

// columnAlign is the alignment declared for column i, or none past the end of
// what the delimiter row described.
func columnAlign(n *gast.Table, i int) gast.Alignment {
	if i < len(n.Alignments) {
		return n.Alignments[i]
	}
	return gast.AlignNone
}

// padCell widens one cell to its column, counting printable runes so the ANSI
// styling inside it does not push the column edges apart.
//
// ponytail: runes, not display cells, matching the rest of the renderer. A
// table of CJK or emoji will sit a column short; swap visibleLen for
// ansi.StringWidth throughout — wrapIndent included — if that day comes.
func padCell(s string, width int, align gast.Alignment) string {
	gap := width - visibleLen(s)
	if gap <= 0 {
		return s
	}
	switch align {
	case gast.AlignRight:
		return strings.Repeat(" ", gap) + s
	case gast.AlignCenter:
		left := gap / 2
		return strings.Repeat(" ", left) + s + strings.Repeat(" ", gap-left)
	default:
		return s + strings.Repeat(" ", gap)
	}
}

// inline renders the inline children of a block: emphasis, code spans, links.
func inline(node ast.Node, src []byte) string {
	var out strings.Builder
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		switch c := child.(type) {
		case *ast.Text:
			out.Write(c.Segment.Value(src))
			if c.SoftLineBreak() || c.HardLineBreak() {
				out.WriteString(" ")
			}
		case *ast.String:
			out.Write(c.Value)
		case *ast.CodeSpan:
			out.WriteString(ansiCode + inline(c, src) + ansiReset)
		case *ast.Emphasis:
			style := ansiItalic
			if c.Level >= 2 {
				style = ansiBold
			}
			out.WriteString(style + inline(c, src) + ansiReset)
		case *ast.Link:
			out.WriteString(ansiUnder + inline(c, src) + ansiReset + ansiDim + " (" + string(c.Destination) + ")" + ansiReset)
		case *ast.AutoLink:
			out.WriteString(ansiUnder + string(c.URL(src)) + ansiReset)
		case *ast.Image:
			out.WriteString(ansiDim + "[image: " + inline(c, src) + "]" + ansiReset)
		case *ast.RawHTML:
			// Model output occasionally contains a stray tag; show it verbatim.
			// An inline node has no Lines() — asking panics — its source is in
			// Segments instead.
			for i := 0; i < c.Segments.Len(); i++ {
				out.Write(segValue(c.Segments.At(i), src))
			}
		case *ast.HTMLBlock:
			out.WriteString(rawLines(child, src))
		case *gast.Strikethrough:
			out.WriteString(ansiStrike + inline(c, src) + ansiReset)
		default:
			out.WriteString(inline(child, src))
		}
	}
	return out.String()
}

func rawLines(node ast.Node, src []byte) string {
	var out strings.Builder
	lines := node.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		out.Write(segValue(seg, src))
	}
	return out.String()
}

func segValue(seg text.Segment, src []byte) []byte { return seg.Value(src) }

// wrapIndent wraps text to width and prefixes every line with indent. ANSI
// escapes are zero-width, so they do not count towards the line length.
func wrapIndent(s string, width int, indent string) string {
	limit := width - visibleLen(indent)
	if limit < 20 {
		limit = 20
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		line := ""
		for _, word := range strings.Fields(para) {
			switch {
			case line == "":
				line = word
			case visibleLen(line)+1+visibleLen(word) <= limit:
				line += " " + word
			default:
				out = append(out, indent+line)
				line = word
			}
		}
		out = append(out, indent+line)
	}
	return strings.Join(out, "\n")
}

// visibleLen counts printable runes, skipping ANSI escape sequences.
func visibleLen(s string) int {
	n, inEscape := 0, false
	for _, r := range s {
		switch {
		case inEscape:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
		case r == 0x1b:
			inEscape = true
		default:
			n++
		}
	}
	return n
}

// PlainText reduces markdown to the prose inside it, using the same parse the
// renderer uses rather than a second guess at the syntax: markers gone, fenced
// code and images dropped whole, every block separated by a space.
//
// Headings come back with a full stop when they have no terminator of their
// own. A heading is a sentence that happens to be short, and without one it
// runs straight into the paragraph under it — for a speech engine that is the
// difference between a pause and a run-on.
func PlainText(source string) string {
	src := []byte(source)
	doc := md.Parser().Parse(text.NewReader(src))
	var blocks []string
	for node := doc.FirstChild(); node != nil; node = node.NextSibling() {
		if s := plainBlock(node, src); s != "" {
			blocks = append(blocks, s)
		}
	}
	return collapse(strings.Join(blocks, " "))
}

// plainBlock is renderBlock without the styling, and without the blocks that
// have nothing a reader wants read to them.
func plainBlock(node ast.Node, src []byte) string {
	switch n := node.(type) {
	case *ast.FencedCodeBlock, *ast.CodeBlock, *ast.HTMLBlock, *ast.ThematicBreak, *gast.Table:
		// Code spoken aloud is punctuation soup and a rule is not a word. A
		// table is worse than either: read row by row it loses the column that
		// gave each cell its meaning, so it is a grid or it is nothing.
		return ""

	case *ast.Heading:
		heading := collapse(plainInline(n, src))
		if heading == "" {
			return ""
		}
		last, _ := utf8.DecodeLastRuneInString(heading)
		if !unicode.IsPunct(last) {
			heading += "."
		}
		return heading

	case *ast.Paragraph, *ast.TextBlock:
		return collapse(plainInline(node, src))

	default:
		// Lists, quotes, list items: containers whose text is in their children.
		var parts []string
		for child := node.FirstChild(); child != nil; child = child.NextSibling() {
			if s := plainBlock(child, src); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	}
}

// plainInline is inline() with every style dropped and the destinations of
// links left out: a spoken URL is noise, and its text already said what it was.
func plainInline(node ast.Node, src []byte) string {
	var out strings.Builder
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		switch c := child.(type) {
		case *ast.Text:
			out.Write(c.Segment.Value(src))
			if c.SoftLineBreak() || c.HardLineBreak() {
				out.WriteString(" ")
			}
		case *ast.String:
			out.Write(c.Value)
		case *ast.AutoLink:
			out.Write(c.URL(src))
		case *ast.Image, *ast.RawHTML:
			// An image has nothing to say out loud, and a stray tag is markup
			// that leaked, not prose.
		default:
			out.WriteString(plainInline(child, src))
		}
	}
	return out.String()
}

var reSpace = regexp.MustCompile(`\s+`)

func collapse(s string) string { return strings.TrimSpace(reSpace.ReplaceAllString(s, " ")) }
