package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newSQLCommand(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sql [directory] <query>",
		Short: "Run raw DuckDB SQL against the loaded logs",
		Long: `Run SQL directly against the logs table.

The table is:

    seq       BIGINT     ingest order
    ts        TIMESTAMP  NULL when the line carried no timestamp
    ts_zoned  BOOLEAN    false when ts came from an assumed timezone
    level     VARCHAR    normalised: trace/debug/info/warn/error/fatal
    message   VARCHAR
    source    VARCHAR    logical source, e.g. checkout-api
    file      VARCHAR    path it was read from
    format    VARCHAR    parser that read it
    line_no   BIGINT
    parsed    BOOLEAN    false when no parser understood the line
    raw       VARCHAR    the original text, always kept
    fields    JSON       unpromoted fields

Reach into the fields bag with DuckDB's JSON operators:

    SELECT fields->>'$.trace_id' AS trace, count(*) FROM logs GROUP BY 1

Parenthesise a JSON extraction before IS NULL or a comparison. DuckDB binds
IS NULL tighter than ->>, so the unparenthesised form is a confusing type
error rather than the filter you meant:

    WHERE (fields->>'$.trace_id') IS NOT NULL      correct
    WHERE fields->>'$.trace_id' IS NOT NULL        parses as fields ->> (... IS NOT NULL)

The filter DSL handles this for you: trace_id:* says the same thing.`,
		Example: `  loupe sql ./logs "SELECT level, count(*) FROM logs GROUP BY 1 ORDER BY 2 DESC"
  loupe sql ./logs "SELECT * FROM logs WHERE ts IS NULL"
  loupe sql "SELECT source, count(*) FROM logs GROUP BY 1"`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, query := ".", args[0]
			if len(args) == 2 {
				path, query = args[0], args[1]
			}
			return runSQL(cmd, g, path, query)
		},
	}
	return cmd
}

func runSQL(cmd *cobra.Command, g *globals, path, query string) error {
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("empty query")
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

	res, err := sess.db.QueryResult(cmd.Context(), g.limit, query)
	if err != nil {
		return err
	}

	return sess.writer.Result(res)
}
