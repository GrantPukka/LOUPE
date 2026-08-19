package render

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/GrantPukka/loupe/internal/parse"
	"github.com/GrantPukka/loupe/internal/store"
)

// ANSI colours. Severity is the only thing colourised, because colour used for
// more than one meaning stops carrying any.
const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiBold   = "\x1b[1m"
	ansiYellow = "\x1b[33m"
	ansiBlue   = "\x1b[34m"
	ansiGrey   = "\x1b[90m"
)

// levelColour maps a severity to its colour. An unrecognised level gets none,
// which is correct: we do not know how serious it is.
func levelColour(level string) string {
	switch level {
	case parse.LevelFatal:
		return ansiBold + ansiRed
	case parse.LevelError:
		return ansiRed
	case parse.LevelWarn:
		return ansiYellow
	case parse.LevelInfo:
		return ansiBlue
	case parse.LevelDebug, parse.LevelTrace:
		return ansiGrey
	default:
		return ""
	}
}

// minColumnWidth stops a squeezed column from collapsing to nothing.
const minColumnWidth = 3

func (w *Writer) table(res store.Result) error {
	if len(res.Columns) == 0 {
		return nil
	}
	if len(res.Rows) == 0 {
		_, err := fmt.Fprintln(w.w, "no "+w.rowMany+" matched")
		return err
	}

	cells := make([][]string, len(res.Rows))
	for i, row := range res.Rows {
		cells[i] = make([]string, len(row))
		for j, v := range row {
			cells[i][j] = sanitise(w.tableValue(v))
		}
	}

	widths := columnWidths(res.Columns, cells, w.opts.Width)
	levelIdx := indexOf(res.Columns, "level")

	// Header, once for a continuous listing and once per result otherwise. A
	// live tail that reprinted its header every time records arrived would
	// read as a stack of tiny tables rather than a log.
	var sb strings.Builder
	if !w.opts.Continuous || !w.headed {
		for i, col := range res.Columns {
			if i > 0 {
				sb.WriteString("  ")
			}
			sb.WriteString(pad(strings.ToUpper(col), widths[i]))
		}
		header := strings.TrimRight(sb.String(), " ")
		if w.opts.Colour {
			header = ansiDim + header + ansiReset
		}
		if _, err := fmt.Fprintln(w.w, header); err != nil {
			return err
		}
		w.headed = true
	}

	for _, row := range cells {
		sb.Reset()
		colour := ""
		if w.opts.Colour && levelIdx >= 0 && levelIdx < len(row) {
			colour = levelColour(row[levelIdx])
		}

		for i, cell := range row {
			if i > 0 {
				sb.WriteString("  ")
			}
			sb.WriteString(pad(truncate(cell, widths[i]), widths[i]))
		}

		line := strings.TrimRight(sb.String(), " ")
		if colour != "" {
			line = colour + line + ansiReset
		}
		if _, err := fmt.Fprintln(w.w, line); err != nil {
			return err
		}
	}

	return w.footer(res)
}

// footer states the counts. Truncation is always declared: output that hides
// its own incompleteness is worse than no output.
func (w *Writer) footer(res store.Result) error {
	if !res.Truncated {
		return nil
	}

	msg := fmt.Sprintf("showing %d of %d %s — use --limit to see more",
		res.RowCount(), res.Total, plural(res.Total, w.rowOne, w.rowMany))
	if w.opts.Colour {
		msg = ansiDim + msg + ansiReset
	}
	_, err := fmt.Fprintln(w.w, msg)
	return err
}

// columnWidths sizes columns to their content, then shrinks the widest ones
// until the row fits.
//
// The widest column is almost always the message, so squeezing widest-first is
// what keeps ts, level, and source intact — those are the columns a reader
// scans down, and truncating them destroys the table's usefulness.
func columnWidths(headers []string, rows [][]string, maxWidth int) []int {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) {
				if n := utf8.RuneCountInString(cell); n > widths[i] {
					widths[i] = n
				}
			}
		}
	}

	const gap = 2
	total := func() int {
		sum := gap * (len(widths) - 1)
		for _, n := range widths {
			sum += n
		}
		return sum
	}

	for total() > maxWidth {
		widest, idx := 0, -1
		for i, n := range widths {
			if n > widest {
				widest, idx = n, i
			}
		}
		if idx < 0 || widest <= minColumnWidth {
			break
		}

		// Take it down to the next-widest rather than one character at a time,
		// so a very wide message column converges in a couple of passes.
		next := minColumnWidth
		for i, n := range widths {
			if i != idx && n > next && n < widest {
				next = n
			}
		}
		reduce := widest - next
		if reduce < 1 {
			reduce = 1
		}
		if over := total() - maxWidth; reduce > over {
			reduce = over
		}
		widths[idx] = widest - reduce
	}

	return widths
}

// truncate shortens a cell to width, marking that it happened.
func truncate(s string, width int) string {
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	if width <= 1 {
		return strings.Repeat("…", width)
	}
	runes := []rune(s)
	return string(runes[:width-1]) + "…"
}

func pad(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

// sanitise makes a value safe to print on one line of a terminal.
//
// Control characters are escaped rather than dropped: a NUL byte from an
// interleaved write is evidence about what happened to the log, and silently
// removing it would hide that.
func sanitise(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return s
	}

	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			sb.WriteString("\\n")
		case r == '\t':
			sb.WriteString("\\t")
		case r == '\r':
			sb.WriteString("\\r")
		case r == 0:
			sb.WriteString("\\0")
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&sb, "\\x%02x", r)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func indexOf(cols []string, name string) int {
	for i, c := range cols {
		if c == name {
			return i
		}
	}
	return -1
}

// plural picks the noun to agree with a count.
func plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// tableValue renders a cell for a person to read rather than for a machine to
// parse back.
//
// The difference is fractions. strconv's shortest form is the shortest string
// that reads back as the same float64, which is exactly right for JSON and CSV
// and wrong on screen: an interpolated p99 of 4963.44 prints as
// 4963.4400000000005, and a reader concludes the tool is broken rather than
// that binary floating point is. The machine formats are untouched, so nothing
// downstream loses a digit.
func (w *Writer) tableValue(v any) string {
	if f, ok := v.(float64); ok {
		return tableFloat(f)
	}
	return w.value(v)
}

// tableFloat trims a float to twelve significant digits, which is past anything
// a log field measures and short of where float64 starts inventing decimals.
func tableFloat(f float64) string {
	exact := strconv.FormatFloat(f, 'f', -1, 64)

	if math.IsNaN(f) || math.IsInf(f, 0) || f == math.Trunc(f) {
		return exact
	}

	rounded, err := strconv.ParseFloat(strconv.FormatFloat(f, 'g', 12, 64), 64)
	if err != nil {
		return exact
	}
	return strconv.FormatFloat(rounded, 'f', -1, 64)
}
