package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GrantPukka/loupe/internal/render"
	"github.com/GrantPukka/loupe/internal/session"
	"github.com/spf13/cobra"
)

func newTopCommand(g *globals) *cobra.Command {
	var (
		limit int
		all   bool
	)

	cmd := &cobra.Command{
		Use:     "top <field> [directory] [filter]",
		Aliases: []string{"facet"},
		Short:   "Count the values of one field, most frequent first",
		Long: `Answer "which endpoints are 500ing?" without dropping to SQL.

Counts every value of a field among the matching records and lists them
descending, with each value's share of the total. A percentage matters here:
412 of 33,000 reads very differently from 412 of 500.

Works on any field the filter language knows — a real column, a field promoted
to a column, or a key still in the JSON bag — and an unknown name gets the same
spelling suggestion it would get in a filter.

Records that carry no value for the field are counted and reported separately
rather than quietly left out of the denominator.`,
		Example: `  loupe top path ./logs
  loupe top status ./logs 'level:>=error'
  loupe top source ./logs --all`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTop(cmd, g, args, limit, all)
		},
	}

	cmd.Flags().IntVar(&limit, "limit", session.DefaultTopLimit,
		"how many values to show, most frequent first")
	cmd.Flags().BoolVar(&all, "all", false, "show every value, however long the tail")

	return cmd
}

func runTop(cmd *cobra.Command, g *globals, args []string, limit int, all bool) error {
	field := args[0]

	given, filter, err := resolveArgs(args[1:])
	if err != nil {
		return err
	}
	paths, _ := resolvePaths(g, given)

	// A breakdown is a whole-dataset question: it cannot report a distribution
	// over records that have not arrived.
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

	set, err := sess.Top(cmd.Context(), plan, session.TopQuery{Field: field, Limit: limit})
	if err != nil {
		return err
	}

	writeTop(os.Stdout, os.Stderr, set, g.quiet)
	return nil
}

// writeTop renders a breakdown.
//
// Values go to stdout so the list can be piped; everything about how to read it
// goes to stderr, so a pipe cannot swallow the caveats.
func writeTop(out, status io.Writer, set session.TopSet, quiet bool) {
	if !quiet {
		fmt.Fprintln(status)
	}

	if set.Present == 0 {
		writeTopEmpty(status, set)
		return
	}

	countWidth, barWidth := 0, 24
	for _, v := range set.Values {
		if n := len(render.Commas(v.Count)); n > countWidth {
			countWidth = n
		}
	}

	// The largest value sets the bar's scale, so the shape of the distribution
	// is visible without reading any numbers. Scaling to the total instead
	// would flatten every bar whenever one value dominates, which is exactly
	// the case worth seeing.
	largest := set.Values[0].Count

	for _, v := range set.Values {
		fmt.Fprintf(out, "%*s  %5.1f%%  %s  %s\n",
			countWidth, render.Commas(v.Count),
			v.Share*100,
			shareBar(v.Count, largest, barWidth),
			displayValue(v.Value))
	}

	writeTopFooter(status, set)
}

func writeTopEmpty(w io.Writer, set session.TopSet) {
	if set.Matched == 0 {
		fmt.Fprintln(w, "No records matched, so there is nothing to break down.")
		return
	}
	fmt.Fprintf(w, "None of the %s carry a value for %s.\n",
		countOf(set.Matched, "matching record", "matching records"), set.Field)
}

func writeTopFooter(w io.Writer, set session.TopSet) {
	fmt.Fprintf(w, "\n%s of %s across %s.\n",
		countOf(set.Distinct, "value", "values"),
		set.Field,
		countOf(set.Present, "record", "records"))

	// Never a bare stop: what is not on screen is counted and named.
	if set.Truncated {
		fmt.Fprintf(w, "%s more not shown, covering %s — use --all.\n",
			countOf(set.Hidden, "value", "values"),
			countOf(set.HiddenRecords, "record", "records"))
	}

	// The denominator is stated, because a share is meaningless without it and
	// records missing the field would otherwise vanish from the arithmetic.
	if set.Absent > 0 {
		fmt.Fprintf(w, "%s matched the filter but carry no %s, so they are outside "+
			"the percentages above (%s:none finds them).\n",
			countOf(set.Absent, "record", "records"), set.Field, set.Field)
	}
}

// shareBar draws a value's count relative to the largest one.
func shareBar(count, largest int64, width int) string {
	if largest <= 0 {
		return strings.Repeat(" ", width)
	}

	filled := int(float64(count) / float64(largest) * float64(width))
	// A non-zero count always draws something, or a single record and none at
	// all look identical.
	if filled == 0 && count > 0 {
		filled = 1
	}
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat(" ", width-filled)
}

// displayValue renders a value so an empty or invisible one is still legible.
//
// A field present but empty is a real answer, and printing nothing would read
// as a rendering fault rather than as the data.
func displayValue(v string) string {
	if v == "" {
		return "(empty)"
	}
	return sanitiseValue(v)
}

// sanitiseValue replaces control characters, which a log line can contain and a
// terminal renders as damage or as nothing at all.
func sanitiseValue(v string) string {
	var b strings.Builder
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			b.WriteString("<ctl>")
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
