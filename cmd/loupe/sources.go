package main

import (
	"database/sql"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func newSourcesCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "sources [directory]",
		Short: "List the log files found, their formats, and their timezone provenance",
		Long: `List every file that was read, with its detected format, record counts, and
whether its timestamps carry a timezone or had one assumed.

The timezone column is the point of this command. A source read under an
assumed zone can be displayed an hour out with nothing warning anybody, so the
assumption is made auditable in ten seconds rather than discovered later.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "."
			if len(args) == 1 {
				path = args[0]
			}
			return runSources(cmd, g, path)
		},
	}
}

func runSources(cmd *cobra.Command, g *globals, path string) error {
	sess, err := g.open(cmd.Context(), path)
	if err != nil {
		return err
	}
	defer sess.Close()

	infos, err := sess.db.Sources(cmd.Context())
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "FILE\tFORMAT\tRECORDS\tUNPARSED\tNO TIMESTAMP\tRANGE\tTIMEZONE")

	for _, si := range infos {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			si.File,
			si.Format,
			si.Records,
			count(si.Unparsed),
			count(si.NoTimestamp),
			timeRange(si.Oldest, si.Newest, sess.loc),
			si.TimezoneStatus(),
		)
	}

	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write table: %w", err)
	}

	for _, skip := range sess.walk.Skipped {
		fmt.Fprintf(os.Stderr, "Skipped %s: %s\n", skip.Path, skip.Reason)
	}
	return nil
}

// count renders zero as a dash, so the numbers that matter stand out.
func count(n int64) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", n)
}

func timeRange(oldest, newest sql.NullTime, loc *time.Location) string {
	if !oldest.Valid || !newest.Valid {
		return "-"
	}

	lo := oldest.Time.In(loc)
	hi := newest.Time.In(loc)

	// Same calendar day is the common case, so do not repeat the date.
	if lo.Format("2006-01-02") == hi.Format("2006-01-02") {
		return fmt.Sprintf("%s %s–%s", lo.Format("2006-01-02"), lo.Format("15:04:05"), hi.Format("15:04:05"))
	}
	return fmt.Sprintf("%s – %s", lo.Format("2006-01-02 15:04:05"), hi.Format("2006-01-02 15:04:05"))
}
