package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/GrantPukka/loupe/internal/render"
	"github.com/GrantPukka/loupe/internal/session"
	"github.com/spf13/cobra"
)

func newFieldsCommand(g *globals) *cobra.Command {
	var (
		limit int
		all   bool
	)

	cmd := &cobra.Command{
		Use:     "fields [directory] [filter]",
		Aliases: []string{"schema"},
		Short:   "List what can be filtered on, and what the values look like",
		Long: `Answer "what is in these logs?" before writing a filter, rather than after
getting one wrong.

Every field a filter can name, with how many records carry it, how many
different values it takes, its type, and a few example values. Fields the
records mostly carry come first.

A filter narrows the question: ` + "`loupe fields ./logs 'level:>=error'`" + ` reports what
the failing records carry, which is usually a shorter and more interesting list
than what everything carries.`,
		Example: `  loupe fields ./logs
  loupe fields ./logs 'level:>=error'
  loupe fields ./logs --all`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFields(cmd, g, args, limit, all)
		},
	}

	cmd.Flags().IntVar(&limit, "limit", session.DefaultFieldLimit,
		"how many fields to show, best covered first")
	cmd.Flags().BoolVar(&all, "all", false, "show every field, however rare")

	return cmd
}

func runFields(cmd *cobra.Command, g *globals, args []string, limit int, all bool) error {
	given, filter, err := resolveArgs(args)
	if err != nil {
		return err
	}
	paths := resolvePaths(g, given)

	// Coverage is a fraction of the whole, so it cannot be reported over
	// records that have not arrived.
	sess, err := g.openBatch(cmd.Context(), paths...)
	if err != nil {
		return err
	}
	defer sess.Close()

	if !g.quiet {
		statusLine(os.Stderr, sess)
	}

	plan, err := sess.Plan(cmd.Context(), filter)
	if err != nil {
		return err
	}

	if !g.quiet {
		timeBanner(os.Stderr, sess, plan)
	}

	if all {
		limit = -1
	}

	set, err := sess.Fields(cmd.Context(), plan, session.FieldQuery{Limit: limit})
	if err != nil {
		return err
	}

	writeFields(os.Stdout, os.Stderr, set, g.quiet)
	return nil
}

// writeFields renders a listing.
//
// The table goes to stdout so it can be piped; the counts and caveats go to
// stderr, like every other listing in the tool.
func writeFields(out, status io.Writer, set session.FieldSet, quiet bool) {
	if !quiet {
		fmt.Fprintln(status)
	}

	if len(set.Fields) == 0 {
		writeFieldsEmpty(status, set)
		return
	}

	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tRECORDS\tCOVERAGE\tDISTINCT\tTYPE\tSTORED\tEXAMPLES")

	for _, f := range set.Fields {
		fmt.Fprintf(tw, "%s\t%s\t%5.1f%%\t%s\t%s\t%s\t%s\n",
			sanitiseValue(f.Name),
			render.Commas(f.Records),
			f.Coverage*100,
			render.Commas(f.Distinct),
			fieldType(f),
			storedIn(f),
			fieldExamples(f))
	}

	if err := tw.Flush(); err != nil {
		fmt.Fprintf(status, "write table: %v\n", err)
		return
	}

	writeFieldsFooter(status, set)
}

func writeFieldsEmpty(w io.Writer, set session.FieldSet) {
	if set.Matched == 0 {
		fmt.Fprintln(w, "No records matched, so there are no fields to describe.")
		return
	}
	fmt.Fprintf(w, "None of the %s carry any field at all.\n",
		countOf(set.Matched, "matching record", "matching records"))
}

func writeFieldsFooter(w io.Writer, set session.FieldSet) {
	fmt.Fprintf(w, "\n%s across %s.\n",
		countOf(int64(len(set.Fields)), "field", "fields"),
		countOf(set.Matched, "matching record", "matching records"))

	// Never a bare stop: what is not on screen is counted and named.
	if set.Truncated {
		fmt.Fprintf(w, "%s not shown — use --limit or --all.\n",
			countOf(set.Hidden, "rarer field", "rarer fields"))
	}

	// "Not in this list" and "not in the data" are very different things to be
	// looking at, and a filtered listing is exactly where they get confused.
	if set.Absent > 0 {
		fmt.Fprintf(w, "%s in this data that no matching record carries, so %s not listed.\n",
			countOf(set.Absent, "field is", "fields are"),
			pluralAre(set.Absent))
	}

	// The warning worth reading twice: an ordering comparison casts, and a
	// value that does not cast is skipped rather than reported.
	for _, f := range set.Fields {
		if !f.PartlyNumeric() {
			continue
		}
		fmt.Fprintf(w, "%s of %s values are numbers, so %s:>N skips the other %s.\n",
			render.Commas(f.Numeric), render.Commas(f.Records),
			f.Name, render.Commas(f.Records-f.Numeric))
	}

	if mixed := mixedFields(set); len(mixed) > 0 {
		fmt.Fprintf(w, "%s more than one type across these records, so a comparison "+
			"silently skips the values that do not fit: %s.\n",
			countOf(int64(len(mixed)), "field holds", "fields hold"),
			strings.Join(mixed, ", "))
	}

	if len(set.Unnameable) > 0 {
		fmt.Fprintf(w, "%s not listed: the name contains a control character, which "+
			"cannot be written into a query — %s.\n",
			countOf(int64(len(set.Unnameable)), "field is", "fields are"),
			strings.Join(set.Unnameable, ", "))
	}

	fmt.Fprintln(w, "msg, line and pattern are accepted as aliases for message, line_no "+
		"and the template id.")
	fmt.Fprintf(w, "Break one down with: loupe top %s\n", set.Fields[0].Name)
}

// mixedFields names the fields that hold more than one type, with the types.
func mixedFields(set session.FieldSet) []string {
	var out []string
	for _, f := range set.Fields {
		if f.Mixed() {
			out = append(out, fmt.Sprintf("%s (%s)", f.Name, strings.Join(f.Types, ", ")))
		}
	}
	return out
}

// fieldType renders the type, marking the ones that are not consistent.
func fieldType(f session.FieldInfo) string {
	switch {
	case f.Type == "":
		return "-"
	case f.Mixed():
		// The full list is in the footer, where there is room for it. Here the
		// point is only that the column cannot be trusted to be one thing.
		return f.Type + " +" + fmt.Sprint(len(f.Types)-1)
	default:
		return f.Type
	}
}

// storedIn says whether the field is a real column or a JSON extraction.
//
// It is in the table rather than the footer because it is the answer to "why is
// this filter slower than that one", and somebody asking that is looking at
// this table already.
func storedIn(f session.FieldInfo) string {
	if f.Column {
		return "column"
	}
	return "bag"
}

// maxExampleRunes is how much of one example value is shown.
//
// An example is for recognising the shape of a field, not for reading. `raw`
// holds whole log lines, and three of those in one cell push every other column
// off the screen — which breaks the table for the thirty-five fields that were
// not the problem.
const maxExampleRunes = 28

// fieldExamples renders a few values, made safe to print and short enough to
// sit in a column.
func fieldExamples(f session.FieldInfo) string {
	if len(f.Examples) == 0 {
		return "-"
	}

	out := make([]string, len(f.Examples))
	for i, v := range f.Examples {
		out[i] = truncateRunes(displayValue(oneLine(v)), maxExampleRunes)
	}
	return strings.Join(out, ", ")
}

// truncateRunes shortens a value, marking that it was shortened.
//
// By runes rather than bytes, or a cut through a multi-byte character produces
// a replacement glyph and the value looks corrupted rather than abbreviated.
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

func pluralAre(n int64) string {
	if n == 1 {
		return "it is"
	}
	return "they are"
}
