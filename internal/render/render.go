package render

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GrantPukka/loupe/internal/store"
)

// Format is an output encoding.
type Format string

const (
	FormatTable  Format = "table"
	FormatJSON   Format = "json"
	FormatNDJSON Format = "ndjson"
	FormatRaw    Format = "raw"
	FormatCSV    Format = "csv"
)

// Formats lists the valid output formats for help text and errors.
var Formats = []Format{FormatTable, FormatJSON, FormatNDJSON, FormatRaw, FormatCSV}

// ParseFormat resolves a --format value.
func ParseFormat(s string) (Format, error) {
	for _, f := range Formats {
		if string(f) == s {
			return f, nil
		}
	}
	names := make([]string, len(Formats))
	for i, f := range Formats {
		names[i] = string(f)
	}
	return "", fmt.Errorf("unknown output format %q (available: %s)", s, strings.Join(names, ", "))
}

// Options configures a renderer.
type Options struct {
	// Format defaults to table on a TTY and ndjson otherwise, so that piping
	// to jq works without a flag.
	Format Format

	// Width is the terminal width used to truncate table output. Zero means
	// detect, and failing that, 120.
	Width int

	// Colour enables ANSI colour. Defaults to on for a TTY.
	Colour bool

	// Location is the display timezone. Timestamps are rendered in it, and it
	// must be stated on screen so nobody has to guess whose clock they are
	// looking at.
	Location *time.Location
}

// Writer renders results.
type Writer struct {
	w    io.Writer
	opts Options
}

// New builds a Writer, filling in defaults from the environment.
func New(w io.Writer, opts Options) *Writer {
	if opts.Format == "" {
		if IsTerminal(w) {
			opts.Format = FormatTable
		} else {
			opts.Format = FormatNDJSON
		}
	}
	if opts.Width <= 0 {
		opts.Width = TerminalWidth(w)
	}
	if opts.Location == nil {
		opts.Location = time.Local
	}
	return &Writer{w: w, opts: opts}
}

// Result renders a query result in the configured format.
func (w *Writer) Result(res store.Result) error {
	switch w.opts.Format {
	case FormatTable:
		return w.table(res)
	case FormatJSON:
		return w.json(res)
	case FormatNDJSON:
		return w.ndjson(res)
	case FormatRaw:
		return w.raw(res)
	case FormatCSV:
		return w.csv(res)
	default:
		return fmt.Errorf("unsupported output format %q", w.opts.Format)
	}
}

// value renders one cell for display.
func (w *Writer) value(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case time.Time:
		return t.In(w.opts.Location).Format("2006-01-02 15:04:05.000")
	case []byte:
		return string(t)
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

// IsTerminal reports whether w is a terminal, which decides the default output
// format and whether colour is used.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// TerminalWidth returns the usable width, defaulting to 120 when it cannot be
// determined.
func TerminalWidth(w io.Writer) int {
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && n > 20 {
		return n
	}
	if !IsTerminal(w) {
		// Piped output should not be wrapped to a guess.
		return 200
	}
	if n := ttyWidth(); n > 20 {
		return n
	}
	return 120
}
