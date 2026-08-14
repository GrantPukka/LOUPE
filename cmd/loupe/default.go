package main

import (
	"fmt"
	"os"

	"github.com/VIGIL-OPS/loupe/internal/query"
	"github.com/spf13/cobra"
)

// The columns the bare `loupe ./logs` view shows.
//
// raw is deliberately absent: it duplicates message for parsed records and
// makes the table unreadable. It is one --format raw away, and always present
// in a handoff.
//
// Ordering puts records with no timestamp last rather than first, then falls
// back to ingest order so their position relative to the surrounding lines in
// their own file is preserved.
const (
	selectClause = `SELECT ts, level, source, message FROM logs WHERE `
	orderClause  = ` ORDER BY ts NULLS LAST, seq`
)

// runDefault handles `loupe`, `loupe ./logs`, and `loupe ./logs '<filter>'`.
//
// Argument order is forgiving on purpose: under time pressure people type the
// filter first about as often as the path.
func runDefault(cmd *cobra.Command, g *globals, args []string) error {
	path, filter := resolveArgs(args)

	// Parse before opening anything. A syntax error should come back
	// immediately rather than after ingesting a gigabyte.
	q, err := query.Parse(filter)
	if err != nil {
		return err
	}

	sess, err := g.open(cmd.Context(), path)
	if err != nil {
		return err
	}
	defer sess.Close()

	if !g.quiet {
		sess.statusLine(os.Stderr)
	}

	sql, err := sess.compile(cmd.Context(), q)
	if err != nil {
		return err
	}

	if !g.quiet {
		sess.timeBanner(os.Stderr)
		fmt.Fprintln(os.Stderr)
	}

	res, err := sess.db.QueryResult(cmd.Context(), g.limit,
		selectClause+sql.Where+orderClause, sql.Args...)
	if err != nil {
		return err
	}

	if err := sess.writer.Result(res); err != nil {
		return err
	}

	// An empty result is where this tool most easily misleads, so explain it
	// rather than leaving the user to guess whether their filter was wrong or
	// their data genuinely contains nothing.
	if res.RowCount() == 0 && !q.IsEmpty() {
		return sess.explainEmpty(cmd.Context(), q)
	}
	return nil
}

// resolveArgs works out which argument is the path and which is the filter.
//
// A path is something that exists on disk. Anything else is a filter, so
// `loupe 'level:error'` in a log directory does what it looks like it should.
func resolveArgs(args []string) (path, filter string) {
	path = "."

	for _, arg := range args {
		if _, err := os.Stat(arg); err == nil {
			path = arg
			continue
		}
		if filter == "" {
			filter = arg
		} else {
			filter += " " + arg
		}
	}
	return path, filter
}
