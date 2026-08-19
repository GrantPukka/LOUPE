package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/GrantPukka/loupe/internal/query"
	"github.com/GrantPukka/loupe/internal/render"
	"github.com/GrantPukka/loupe/internal/session"
	"github.com/spf13/cobra"
)

// runStats answers a filter that carries a `stats` clause.
//
// It is not a subcommand. Aggregation is part of the filter language, so
// `loupe ./logs 'level:>=error stats count() by path'` reads as one question
// rather than as a different tool, and the same string works in the UI's filter
// box and in a saved query.
func runStats(cmd *cobra.Command, g *globals, sess *session.Session, plan session.Plan) error {
	if g.follow {
		return fmt.Errorf("--follow lists records as they arrive; `%s` summarises a finished read\n"+
			"drop --follow, or drop the stats clause to tail the same filter",
			plan.Query.Stats)
	}
	if g.handoff != "" {
		return fmt.Errorf("--handoff exports the records behind a filter, not a summary of them\n" +
			"drop the stats clause to hand off the matching records")
	}

	set, err := sess.Stats(cmd.Context(), plan, session.StatsQuery{Limit: g.limit})
	if err != nil {
		return err
	}

	writer, err := g.statsRenderer(sess.Loc)
	if err != nil {
		return err
	}

	if !g.quiet {
		fmt.Fprintln(os.Stderr)
	}
	if err := writer.Result(set.Result); err != nil {
		return err
	}

	writeStatsFooter(os.Stderr, set, sess.Loc, g.quiet)
	return nil
}

// statsRenderer builds a renderer that knows its rows are groups.
//
// The truncation footer names what it cut, and calling a group a record would
// misstate the size of what is missing: twenty of four thousand groups is not
// twenty of four thousand records.
func (g *globals) statsRenderer(loc *time.Location) (*render.Writer, error) {
	writer, err := g.renderer(loc)
	if err != nil {
		return nil, err
	}
	writer.SetRowNoun("group", "groups")
	return writer, nil
}

// writeStatsFooter states what the table is a summary of.
//
// Everything here goes to stderr, so piping the table into another tool cannot
// swallow the caveats that make it readable.
func writeStatsFooter(w io.Writer, set session.StatsSet, loc *time.Location, quiet bool) {
	if quiet {
		return
	}
	fmt.Fprintln(w)

	if set.Matched == 0 {
		fmt.Fprintln(w, "No records matched, so there is nothing to summarise.")
		return
	}

	rows := int64(set.Result.RowCount())
	if set.Result.Truncated {
		rows = set.Result.Total
	}

	if set.Grouped == set.Matched {
		fmt.Fprintf(w, "%s over %s.\n",
			countOf(rows, "group", "groups"),
			countOf(set.Matched, "matching record", "matching records"))
	} else {
		// The two denominators differ, so both are given. A summary of 3,317
		// of 3,405 matching records is a different claim from a summary of all
		// of them, and the lines below say where the rest went.
		fmt.Fprintf(w, "%s over %s of %s.\n",
			countOf(rows, "group", "groups"),
			render.Commas(set.Grouped),
			countOf(set.Matched, "matching record", "matching records"))
	}

	if set.Bin > 0 {
		fmt.Fprintf(w, "Buckets are %s wide, anchored to %s = %s.\n",
			query.FormatDuration(set.Bin), formatInstant(set.Origin, loc), formatInstantUTC(set.Origin))
	}

	for _, absent := range set.Absent {
		fmt.Fprintf(w, "%s matched the filter but carry no %s, so they are in no group "+
			"(%s:none finds them).\n",
			countOf(absent.Count, "record", "records"), absent.Field, absent.Field)
	}

	if set.EmptyBins > 0 {
		fmt.Fprintf(w, "%s between the first and the last hold no matching record, "+
			"so they have no row here — `loupe histogram` draws the gaps.\n",
			countOf(set.EmptyBins, "bucket", "buckets"))
	}

	if set.NoTimestamp > 0 {
		fmt.Fprintf(w, "%s matched the filter but carry no timestamp, so they fall in no "+
			"bucket (ts:none finds them).\n",
			countOf(set.NoTimestamp, "record", "records"))
	}

	for _, note := range set.Notes {
		fmt.Fprintf(w, "%s\n", note)
	}
}

// formatInstant renders one instant in the display timezone, named.
func formatInstant(t time.Time, loc *time.Location) string {
	local := t.In(loc)
	zone, _ := local.Zone()
	return local.Format("2006-01-02 15:04:05") + " " + zone
}

func formatInstantUTC(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05") + " UTC"
}
