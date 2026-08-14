package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// defaultColumns are what the bare `loupe ./logs` view shows.
//
// raw is deliberately absent: it duplicates message for parsed records and
// makes the table unreadable. It is one --format raw away, and always present
// in a handoff.
const defaultQuery = `
	SELECT ts, level, source, message
	FROM logs
	ORDER BY ts NULLS LAST, seq`

// runDefault handles `loupe`, `loupe ./logs`, and `loupe ./logs '<filter>'`.
//
// Argument order is forgiving on purpose: under time pressure people type the
// filter first about as often as the path.
func runDefault(cmd *cobra.Command, g *globals, args []string) error {
	path, filter := resolveArgs(args)

	if filter != "" {
		// The filter DSL lands in the next milestone. Say so precisely rather
		// than silently ignoring the argument and showing everything, which
		// would be a wrong answer presented confidently.
		return fmt.Errorf("filter expressions are not implemented yet; "+
			"use `loupe sql %q \"SELECT ...\"` for now", path)
	}

	sess, err := g.open(cmd.Context(), path)
	if err != nil {
		return err
	}
	defer sess.Close()

	if !g.quiet {
		sess.statusLine(os.Stderr)
		fmt.Fprintln(os.Stderr)
	}

	res, err := sess.db.QueryResult(cmd.Context(), g.limit, defaultQuery)
	if err != nil {
		return err
	}

	return sess.writer.Result(res)
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
