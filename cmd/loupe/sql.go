package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/GrantPukka/loupe/internal/render"
	"github.com/spf13/cobra"
)

// explainSQLError turns DuckDB's binder errors into ones that name a way
// forward.
//
// The timezone case is the one worth catching. The embedded DuckDB has no ICU
// extension, so AT TIME ZONE does not bind, and installing one would mean an
// outbound request — which this tool does not make, and not making it is most
// of why it can be trusted with production logs. The raw error names a function
// signature and leaves the reader to work out that a whole class of query is
// unavailable, when in fact the tool already does the conversion they were
// reaching for.
func explainSQLError(err error, loc *time.Location) error {
	msg := err.Error()
	if !strings.Contains(msg, "timezone(") && !strings.Contains(msg, "AT TIME ZONE") {
		return err
	}

	return fmt.Errorf("%w\n\n"+
		"AT TIME ZONE needs DuckDB's ICU extension, which is not built in — "+
		"loading one would mean an outbound request, and loupe makes none.\n"+
		"loupe already converts for you: ts is shown in %s, and --tz or --utc "+
		"changes that for the whole session.\n"+
		"For arithmetic on another zone, work in UTC and offset it yourself", err, loc)
}

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

	sess, err := g.openBatch(cmd.Context(), path)
	if err != nil {
		return err
	}
	defer sess.Close()

	if !g.quiet {
		statusLine(os.Stderr, sess)
		fmt.Fprintln(os.Stderr)
	}

	res, err := sess.DB.QueryResult(cmd.Context(), g.limit, query)
	if err != nil {
		return explainSQLError(err, sess.Loc)
	}

	writer, opts, err := g.sqlRenderer(sess.Loc)
	if err != nil {
		return err
	}

	// Every conversion in this tool is announced. A conversion the reader is
	// entitled to expect and does not get has to be announced too, or the
	// column silently reads as though it were in the display zone.
	if cols := render.VerbatimTimestamps(opts, res); len(cols) > 0 && !g.quiet {
		fmt.Fprintf(os.Stderr,
			"Shown exactly as computed, not converted to %s: %s. "+
				"Only ts is known to hold UTC.\n\n",
			sess.Loc, strings.Join(cols, ", "))
	}

	return writer.Result(res)
}
