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

	// Continuous marks output that arrives in batches but is one listing: a
	// live tail, or a stream being read as it is written. The table header is
	// printed once rather than once per batch, which is the difference between
	// a log view and a stack of tiny tables.
	Continuous bool

	// UserSQL marks a result whose columns were chosen by the user rather than
	// by loupe's own compiler, which changes what a TIMESTAMP in it means.
	//
	// A DuckDB TIMESTAMP is a naive value: it carries no zone. Loupe's own
	// queries only ever produce one from ts, which holds UTC, so converting it
	// into the display timezone is right and is what every other part of this
	// tool promises. A value the user computed in `loupe sql` was never UTC,
	// and shifting it by the display offset moved literal timestamps ten hours
	// and a day, silently, in the one command whose whole purpose is answering
	// what the DSL cannot.
	//
	// So under this flag only ts, and anything DuckDB actually typed as
	// TIMESTAMP WITH TIME ZONE, is converted. See VerbatimTimestamps.
	UserSQL bool
}

// Writer renders results.
type Writer struct {
	w    io.Writer
	opts Options
	// headed records that a continuous listing has already printed its header.
	headed bool
	// rowOne and rowMany name what a row is, for the truncation footer.
	rowOne, rowMany string
}

// SetRowNoun names what one row of the result is.
//
// It exists for the one caller whose rows are not records: an aggregation's
// rows are groups, and a footer saying "showing 20 of 4,132 records" would
// misstate the size of what the limit cut off.
func (w *Writer) SetRowNoun(one, many string) {
	w.rowOne, w.rowMany = one, many
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
	return &Writer{w: w, opts: opts, rowOne: "record", rowMany: "records"}
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
//
// localise says whether a timestamp in this column is a real instant, and so
// whether showing it in the display timezone is a conversion or a corruption.
// See Options.UserSQL and localisedColumns.
func (w *Writer) value(v any, localise bool) string {
	switch t := v.(type) {
	case nil:
		return ""
	case time.Time:
		if localise {
			t = t.In(w.opts.Location)
		}
		return t.Format("2006-01-02 15:04:05.000")
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

// localisedColumns decides, per column, whether a timestamp found in it is a
// real UTC instant.
//
// Loupe's own compiler only ever derives a timestamp from ts, so everything it
// produces is one. User SQL is the exception, and there the only column loupe
// can vouch for is ts itself — plus anything DuckDB has explicitly typed as
// carrying a zone, which is unambiguous whoever wrote it.
func (w *Writer) localisedColumns(res store.Result) []bool {
	out := make([]bool, len(res.Columns))
	for i := range res.Columns {
		switch {
		case i < len(res.Types) && zoned(res.Types[i]):
			out[i] = true
		case !w.opts.UserSQL:
			out[i] = true
		default:
			out[i] = res.Columns[i] == "ts"
		}
	}
	return out
}

// zoned reports whether a DuckDB type carries a timezone of its own.
func zoned(dbType string) bool {
	upper := strings.ToUpper(dbType)
	return strings.Contains(upper, "WITH TIME ZONE") || strings.Contains(upper, "TIMESTAMPTZ")
}

// VerbatimTimestamps names the timestamp columns a result will show exactly as
// computed, without converting them into the display timezone.
//
// Every other conversion in this tool is announced, and a conversion that does
// *not* happen where the reader expects one has to be announced too. `loupe
// sql` prints this above its table; it is empty for every other caller, whose
// timestamps are all real instants.
func VerbatimTimestamps(opts Options, res store.Result) []string {
	if !opts.UserSQL {
		return nil
	}

	var out []string
	for i, col := range res.Columns {
		if col == "ts" || i >= len(res.Types) || zoned(res.Types[i]) {
			continue
		}
		if !strings.HasPrefix(strings.ToUpper(res.Types[i]), "TIMESTAMP") &&
			!strings.EqualFold(res.Types[i], "DATE") {
			continue
		}
		out = append(out, col)
	}
	return out
}
