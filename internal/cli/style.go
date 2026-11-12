package cli

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// style is how table output looks on its stream. It follows the conventions of Circuit's command line: on a terminal,
// tables have thin box lines and symbols mark a route's state; colour only on a terminal, and never with NO_COLOR set.
// Each colour has one meaning: red for what stops traffic or an error, yellow for a warning, green for what works,
// dim for detail, bold for headings. Anything else, such as a pipe, gets plain text as before. Words such as status
// and reason names are never changed, only painted.
type style struct {
	// terminal is true when the stream is a terminal.
	terminal bool
	// color is true on a terminal without NO_COLOR.
	color bool
	// width is the terminal's width in columns, or 0 when unknown.
	width int
}

// detectTerminal reports whether w is a terminal, and its width. Tests replace it.
func detectTerminal(w io.Writer) (bool, int) {
	f, ok := w.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return false, 0
	}
	width, _, err := term.GetSize(int(f.Fd()))
	if err != nil {
		width = 0
	}
	return true, width
}

// styleFor returns the style of a stream.
func (o *options) styleFor(w io.Writer) style {
	detect := o.terminal
	if detect == nil {
		detect = detectTerminal
	}
	tty, width := detect(w)
	if !tty {
		return style{}
	}
	// NO_COLOR set to an empty string counts as unset (no-color.org).
	return style{terminal: true, color: os.Getenv("NO_COLOR") == "", width: width}
}

func (s style) paint(code, reset, text string) string {
	if !s.color || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[" + reset + "m"
}

func (s style) bold(t string) string   { return s.paint("1", "22", t) }
func (s style) dim(t string) string    { return s.paint("2", "22", t) }
func (s style) red(t string) string    { return s.paint("31", "39", t) }
func (s style) green(t string) string  { return s.paint("32", "39", t) }
func (s style) yellow(t string) string { return s.paint("33", "39", t) }

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// visible returns how many columns text takes: colours take none.
func visible(text string) int {
	return utf8.RuneCountInString(ansi.ReplaceAllString(text, ""))
}

// pad fills text with spaces to width columns, on the left when right is true.
func pad(text string, width int, right bool) string {
	fill := strings.Repeat(" ", max(0, width-visible(text)))
	if right {
		return fill + text
	}
	return text + fill
}

// wrap splits text into lines of at most width columns at spaces, and cuts a longer word. Colours are dropped, since
// a cut could break one.
func wrap(text string, width int) []string {
	if width <= 0 || visible(text) <= width {
		return []string{text}
	}
	var lines []string
	line := ""
	for _, word := range strings.Fields(ansi.ReplaceAllString(text, "")) {
		next := word
		if line != "" {
			next = line + " " + word
		}
		if utf8.RuneCountInString(next) <= width {
			line = next
			continue
		}
		if line != "" {
			lines = append(lines, line)
		}
		for utf8.RuneCountInString(word) > width {
			r := []rune(word)
			lines = append(lines, string(r[:width]))
			word = string(r[width:])
		}
		line = word
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// column is a column of a box table.
type column struct {
	title string
	// right lines up the column's values on the right, as numbers and ages.
	right bool
}

// boxTable prints rows in a table with thin lines around each cell and a bold heading row. In a terminal too narrow for
// it, the last column's text goes on more lines.
func (s style) boxTable(w io.Writer, columns []column, rows [][]string) {
	widths := make([]int, len(columns))
	for i, c := range columns {
		widths[i] = visible(c.title)
		for _, r := range rows {
			widths[i] = max(widths[i], visible(r[i]))
		}
	}
	// The borders and the padding take 3 columns a column, and 1 more.
	last := len(columns) - 1
	if room := s.width - 1 - 3*len(columns); s.width > 0 {
		others := 0
		for _, n := range widths[:last] {
			others += n
		}
		if others+widths[last] > room {
			widths[last] = min(widths[last], max(visible(columns[last].title), room-others, 12))
		}
	}
	line := func(left, middle, right string) {
		parts := make([]string, len(widths))
		for i, n := range widths {
			parts[i] = strings.Repeat("─", n+2)
		}
		fmt.Fprintln(w, s.dim(left+strings.Join(parts, middle)+right))
	}
	bar := s.dim("│")
	row := func(cells []string) {
		wrapped := make([][]string, len(cells))
		height := 1
		for i, text := range cells {
			wrapped[i] = []string{text}
			if i == last {
				wrapped[i] = wrap(text, widths[i])
			}
			height = max(height, len(wrapped[i]))
		}
		for h := range height {
			parts := make([]string, len(cells))
			for i := range cells {
				text := ""
				if h < len(wrapped[i]) {
					text = wrapped[i][h]
				}
				parts[i] = pad(text, widths[i], columns[i].right)
			}
			fmt.Fprintln(w, bar+" "+strings.Join(parts, " "+bar+" ")+" "+bar)
		}
	}
	line("┌", "┬", "┐")
	titles := make([]string, len(columns))
	for i, c := range columns {
		titles[i] = s.bold(c.title)
	}
	row(titles)
	line("├", "┼", "┤")
	for _, r := range rows {
		row(r)
	}
	line("└", "┴", "┘")
}

// fields prints name and value pairs with the names dim, and the values lined up by the columns they take.
func (s style) fields(w io.Writer, pairs ...[2]string) {
	width := 0
	for _, p := range pairs {
		width = max(width, utf8.RuneCountInString(p[0])+1)
	}
	for _, p := range pairs {
		fmt.Fprintf(w, "%s  %s\n", s.dim(pad(p[0]+":", width, false)), p[1])
	}
}
