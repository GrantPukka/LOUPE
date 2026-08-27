package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GrantPukka/loupe/internal/query"
	"github.com/GrantPukka/loupe/internal/render"
	"github.com/GrantPukka/loupe/internal/session"
	"github.com/spf13/cobra"
)

func newDiffCommand(g *globals) *cobra.Command {
	var (
		before string
		after  string
		limit  int
		all    bool
	)

	cmd := &cobra.Command{
		Use:     "diff [directory] [filter]",
		Aliases: []string{"compare"},
		Short:   "Compare two time windows and report what is different",
		Long: `Answer "what is different between the healthy window and the incident window".

Both windows are written in the filter language's own time grammar, so
--before 13:00-14:00 means the same thing here as 13:00-14:00 does in a filter,
resolved against the same data and reported in the same two timezones.

Message templates are compared: which shapes appeared, which vanished, and which
are happening at a different rate. A filter given alongside applies to both
windows, so a comparison can be narrowed to one source without writing it twice.

Differences are ranked by how surprising they are rather than by raw delta. A
template that doubled from 2 to 4 is noise; one that went from nothing to 300 is
the incident.`,
		Example: `  loupe diff ./logs --before 13:00-14:00 --after 14:00-15:00
  loupe diff ./logs --before yesterday --after last:1h
  loupe diff ./logs 'source:checkout-api' --before on:2026-08-12 --after on:2026-08-13`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiff(cmd, g, args, before, after, limit, all)
		},
	}

	cmd.Flags().StringVar(&before, "before", "", "the window to compare from, e.g. 13:00-14:00")
	cmd.Flags().StringVar(&after, "after", "", "the window to compare to, e.g. 14:00-15:00")
	cmd.Flags().IntVar(&limit, "limit", session.DefaultDiffLimit,
		"how many differences to show, most surprising first")
	cmd.Flags().BoolVar(&all, "all", false, "show every difference, however small")

	return cmd
}

func runDiff(cmd *cobra.Command, g *globals, args []string, before, after string, limit int, all bool) error {
	given, filter, err := resolveArgs(args)
	if err != nil {
		return err
	}
	paths := resolvePaths(g, given)

	// A comparison is a whole-dataset question: it cannot say what is
	// different about a window whose records have not arrived.
	sess, err := g.openBatch(cmd.Context(), paths...)
	if err != nil {
		return err
	}
	defer sess.Close()

	if !g.quiet {
		statusLine(os.Stderr, sess)
	}

	if all {
		limit = -1
	}

	set, err := sess.Diff(cmd.Context(), session.DiffQuery{
		Before: before,
		After:  after,
		Filter: filter,
		Limit:  limit,
	})
	if err != nil {
		return err
	}

	writeDiff(os.Stdout, os.Stderr, set, sess.Loc, g.quiet)
	return nil
}

// writeDiff renders a comparison.
//
// The differences go to stdout so the list can be piped; the windows and every
// caveat about how to read them go to stderr, so a pipe cannot swallow the
// context that makes the numbers mean anything.
func writeDiff(out, status io.Writer, set session.DiffSet, loc *time.Location, quiet bool) {
	if !quiet {
		writeDiffWindows(status, set, loc)
		fmt.Fprintln(status)
	}

	if len(set.Items) == 0 {
		writeDiffEmpty(status, set)
		return
	}

	unit := rateUnit(set)
	rows := make([][3]string, len(set.Items))
	beforeWidth, afterWidth, changeWidth := len("BEFORE"), len("AFTER"), len("CHANGE")

	for i, item := range set.Items {
		rows[i] = [3]string{
			diffCount(item.Before, item.BeforeRate, set.Rates, unit),
			diffCount(item.After, item.AfterRate, set.Rates, unit),
			diffChange(item),
		}
		beforeWidth = max(beforeWidth, len(rows[i][0]))
		afterWidth = max(afterWidth, len(rows[i][1]))
		changeWidth = max(changeWidth, len(rows[i][2]))
	}

	// The headings go to stderr with the rest of the context, so a piped list
	// is data and nothing else. They are sized to the columns below, so the two
	// streams line up on a terminal where they interleave.
	if !quiet {
		fmt.Fprintf(status, "%*s  %*s  %*s  %s\n",
			beforeWidth, "BEFORE", afterWidth, "AFTER", changeWidth, "CHANGE", "WHAT")
	}

	for i, item := range set.Items {
		fmt.Fprintf(out, "%*s  %*s  %*s  %s\n",
			beforeWidth, rows[i][0],
			afterWidth, rows[i][1],
			changeWidth, rows[i][2],
			diffLabel(item))
	}

	writeDiffFooter(status, set, unit)
}

// writeDiffWindows states both windows in both timezones, with their lengths.
//
// docs/FILTER-DSL.md section 2.3 makes this the feature rather than a nicety,
// and it matters more here than anywhere: every number below is a comparison
// between these two spans, and a reader who has the wrong idea of either one
// draws the wrong conclusion from all of them.
func writeDiffWindows(w io.Writer, set session.DiffSet, loc *time.Location) {
	for _, side := range []struct {
		name   string
		window session.DiffWindow
	}{{"before", set.Before}, {"after", set.After}} {
		fmt.Fprintf(w, "%-6s  %s\n", side.name, side.window.Interval.Describe(loc))
		fmt.Fprintf(w, "        %s · %s\n",
			formatWindowLength(side.window.Duration()),
			countOf(side.window.Records, "record", "records"))

		for _, note := range side.window.Notes {
			fmt.Fprintf(w, "        Note: %s\n", note)
		}
	}

	// The change in volume, once, before the table. Everything below is ranked
	// on what is different *beyond* this, so a reader who does not know it has
	// no way to read the rest.
	if line := describeVolume(set); line != "" {
		fmt.Fprintf(w, "\n%s\n", line)
	}
}

// describeVolume states how much more, or less, was logged.
func describeVolume(set session.DiffSet) string {
	before, after := set.Before.Records, set.After.Records
	switch {
	case before == 0 || after == 0:
		return ""
	case before == after:
		return fmt.Sprintf("Volume is unchanged at %s per window.", render.Commas(before))
	}

	change := fmt.Sprintf("%+.0f%%", (float64(after)/float64(before)-1)*100)
	if ratio := float64(after) / float64(before); ratio >= 2 {
		change = "×" + strconv.FormatFloat(ratio, 'f', ratioDecimals(ratio), 64)
	}

	// Rates rather than counts when the windows are different lengths, or the
	// multiplier would be of two things that are not comparable.
	if set.Rates {
		return fmt.Sprintf("Volume went from %s to %s records — %s per unit time. "+
			"Everything below is ranked on what changed beyond that.",
			render.Commas(before), render.Commas(after),
			rateRatio(set))
	}
	return fmt.Sprintf("Volume went from %s to %s records (%s). "+
		"Everything below is ranked on what changed beyond that.",
		render.Commas(before), render.Commas(after), change)
}

// rateRatio expresses the volume change as a change in rate, for windows of
// unequal length.
func rateRatio(set session.DiffSet) string {
	beforeRate := float64(set.Before.Records) / set.Before.Duration().Seconds()
	afterRate := float64(set.After.Records) / set.After.Duration().Seconds()
	if beforeRate <= 0 {
		return "a new rate"
	}

	ratio := afterRate / beforeRate
	if ratio >= 2 {
		return "×" + strconv.FormatFloat(ratio, 'f', ratioDecimals(ratio), 64)
	}
	return fmt.Sprintf("%+.0f%%", (ratio-1)*100)
}

func writeDiffEmpty(w io.Writer, set session.DiffSet) {
	switch {
	case set.Before.Records == 0 && set.After.Records == 0:
		fmt.Fprintln(w, "Neither window matched a record, so there is nothing to compare.")

	// One empty window is not a comparison. Every template and every value in
	// the other one would be "new", which is true and says nothing: the answer
	// is the listing, and this points at it rather than printing it as a
	// thousand rows all equally surprising.
	case set.Before.Records == 0:
		fmt.Fprintf(w, "The before window matched no records, so there is nothing to "+
			"compare against — everything in the after window is new.\n"+
			"List it with: loupe <dir> '%s'\n", set.After.Filter)
	case set.After.Records == 0:
		fmt.Fprintf(w, "The after window matched no records, so everything in the before "+
			"window has stopped.\nList what was there with: loupe <dir> '%s'\n",
			set.Before.Filter)

	case set.Compared == 0:
		fmt.Fprintln(w, "Nothing in either window to compare.")
	default:
		fmt.Fprintf(w, "Nothing differs: everything compared makes up the same share of "+
			"both windows (%s).\n", describeDiffCounts(set))
	}
	writeDiffCaveats(w, set)
}

func writeDiffFooter(w io.Writer, set session.DiffSet, unit rateScale) {
	fmt.Fprintf(w, "\n%s differ, most surprising first.\n", describeDiffCounts(set))

	if set.Rates {
		fmt.Fprintf(w, "The windows are different lengths (%s and %s), so the columns are "+
			"%s, not counts.\n",
			formatWindowLength(set.Before.Duration()),
			formatWindowLength(set.After.Duration()),
			unit.describe())
	}

	// Never a bare stop: what the limit cut is counted and named.
	if set.Truncated {
		fmt.Fprintf(w, "%s not shown — use --limit or --all.\n",
			countOf(set.Hidden, "smaller difference", "smaller differences"))
	}

	writeDiffCaveats(w, set)

	if len(set.Items) > 0 {
		fmt.Fprintf(w, "Expand one with: loupe <dir> '%s'\n", diffFilterFor(set.Items[0]))
	}
}

// diffFilterFor is the filter that shows the records behind a difference.
//
// Kind-aware, because the top of the list is as likely to be a field value as a
// template and offering `pattern:level` for a field would be a filter that
// finds nothing.
func diffFilterFor(item session.DiffItem) string {
	switch item.Kind {
	case session.DiffPattern:
		return "pattern:" + item.Key
	case session.DiffField:
		return item.Key + ":*"
	default:
		// The label already reads field=value, which is the filter, except that
		// a value needing quotes has to keep them.
		field, value, _ := strings.Cut(item.Label, "=")
		return field + ":" + quoteFilterValue(value)
	}
}

// quoteFilterValue puts a value back in the form the filter language reads.
func quoteFilterValue(v string) string {
	if v == "" || strings.ContainsAny(v, ` "	,:`) {
		return strconv.Quote(v)
	}
	return v
}

// describeDiffCounts states what was looked at and how much of it moved,
// broken down by kind.
//
// One number would hide which denominator the answer came from: four templates
// and forty thousand field values are both "compared", and "3 of 4" says
// something that "3 of 40,004" does not.
func describeDiffCounts(set session.DiffSet) string {
	parts := make([]string, 0, len(set.Counts))
	for _, c := range set.Counts {
		parts = append(parts, fmt.Sprintf("%s of %s %s",
			render.Commas(c.Differed), render.Commas(c.Compared), c.Kind.Plural()))
	}

	switch len(parts) {
	case 0:
		return "nothing to compare"
	case 1:
		return parts[0]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1]
	}
}

// writeDiffCaveats states the things that make the comparison less than a
// clean split of the data.
func writeDiffCaveats(w io.Writer, set session.DiffSet) {
	if !set.Overlap.Unbounded() && !set.Overlap.Empty() {
		fmt.Fprintf(w, "The windows overlap; a record inside the overlap is counted on "+
			"both sides, so each column still matches what that window alone would report.\n")
	}
	if set.NoTimestamp > 0 {
		fmt.Fprintf(w, "%s %s no timestamp and are in neither window (ts:none finds them).\n",
			countOf(set.NoTimestamp, "record", "records"), have(set.NoTimestamp))
	}

	if len(set.Unnameable) > 0 {
		fmt.Fprintf(w, "%s not compared at all: the name contains a control character, "+
			"which cannot be written into a query — %s.\n",
			countOf(int64(len(set.Unnameable)), "field was", "fields were"),
			strings.Join(set.Unnameable, ", "))
	}
	// One sentence for all of them. Six near-identical lines about six
	// identifier fields is a footer nobody reads to the end of, and the end is
	// where the counts that matter live.
	if len(set.Skipped) > 0 {
		named := make([]string, len(set.Skipped))
		for i, skipped := range set.Skipped {
			named[i] = fmt.Sprintf("%s (%s)", skipped.Field, render.Commas(skipped.Distinct))
		}
		fmt.Fprintf(w, "%s too many distinct values to compare one by one, so only whether "+
			"a record carries them was compared: %s.\n",
			countOf(int64(len(set.Skipped)), "field has", "fields have"),
			strings.Join(named, ", "))
	}
}

// rateScale is the unit rates are shown in.
type rateScale struct {
	name string
	per  float64
}

var (
	perSecond = rateScale{"/s", 1}
	perMinute = rateScale{"/min", 60}
	perHour   = rateScale{"/h", 3600}
)

func (r rateScale) describe() string {
	switch r {
	case perSecond:
		return "rates per second"
	case perHour:
		return "rates per hour"
	default:
		return "rates per minute"
	}
}

// rateUnit picks the unit that makes the largest number on screen readable.
//
// The smallest unit in which something reaches one, so a burst measured in
// hundreds a second is not written as hundreds of thousands an hour and a slow
// trickle is not written as 0.003 a second.
func rateUnit(set session.DiffSet) rateScale {
	var largest float64
	for _, item := range set.Items {
		largest = max(largest, max(item.BeforeRate, item.AfterRate))
	}

	switch {
	case largest >= 1:
		return perSecond
	case largest*60 >= 1:
		return perMinute
	default:
		return perHour
	}
}

// diffCount renders one side of an item.
//
// Counts when the windows are the same length, because that is what people
// think in and the two are directly comparable. Rates when they are not, since
// 100 records in five minutes and 100 in an hour are not the same event.
func diffCount(n int64, rate float64, rates bool, unit rateScale) string {
	if !rates {
		return render.Commas(n)
	}
	return formatRate(rate*unit.per) + unit.name
}

func formatRate(v float64) string {
	switch {
	case v == 0:
		return "0"
	case v >= 100:
		return render.Commas(int64(v + 0.5))
	case v >= 10:
		return strconv.FormatFloat(v, 'f', 1, 64)
	default:
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
}

// diffChange names what happened, in the form that reads fastest.
//
// A percentage for a modest move, a multiplier once something more than
// doubled: "×14" lands where "+1,300%" has to be decoded.
func diffChange(item session.DiffItem) string {
	switch item.Change {
	case session.DiffAppeared:
		return "new"
	case session.DiffVanished:
		return "gone"
	}

	change, ok := item.RateChange()
	if !ok {
		return "new"
	}

	if ratio := 1 + change; ratio >= 2 {
		return "×" + strconv.FormatFloat(ratio, 'f', ratioDecimals(ratio), 64)
	}
	return fmt.Sprintf("%+.0f%%", change*100)
}

// ratioDecimals keeps a multiplier short: ×2.4 is worth a decimal, ×1,200 is
// not.
func ratioDecimals(ratio float64) int {
	if ratio < 10 {
		return 1
	}
	return 0
}

// diffLabel renders what the item is.
//
// Both halves are sanitised. A field value comes straight out of a log file and
// can hold a NUL, which a terminal renders as nothing at all — so a corrupted
// path reads as a spacing bug in loupe rather than as damage in the data. This
// is the same call EC002 and EC005 made, for the same reason.
func diffLabel(item session.DiffItem) string {
	if item.Detail == "" {
		return sanitiseValue(item.Label)
	}
	return sanitiseValue(item.Label) + "  " + sanitiseValue(oneLine(item.Detail))
}

// formatWindowLength renders a duration the way the filter language spells one,
// so the length of a window and the window itself are written alike: 30m, not
// 30m0s.
//
// Anything that is not a whole number of seconds falls back to the standard
// form rather than inventing a spelling the language would not accept.
func formatWindowLength(d time.Duration) string {
	if d <= 0 {
		return "no length"
	}

	// Rounded to the second before rendering. A window whose open end was
	// bounded by the newest record carries a one-nanosecond tail, because an
	// interval is half-open and the newest record has to fall inside it — and
	// "2h0m0.000000001s" is not a length anybody wants to read. Only the label
	// rounds; the rate arithmetic uses the exact span.
	if d >= time.Minute {
		d = d.Round(time.Second)
	}
	if d%time.Second == 0 {
		return query.FormatDuration(d)
	}
	return d.String()
}
