package main

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/GrantPukka/loupe/internal/parse"
	"github.com/GrantPukka/loupe/internal/store"
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
	sess, err := g.openBatch(cmd.Context(), path)
	if err != nil {
		return err
	}
	defer sess.Close()

	infos, err := sess.DB.Sources(cmd.Context())
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
			timeRange(si.Oldest, si.Newest, sess.Loc),
			si.TimezoneStatus(),
		)
	}

	if err := tw.Flush(); err != nil {
		return fmt.Errorf("write table: %w", err)
	}

	warnUnparsed(os.Stderr, infos)

	for _, skip := range sess.Walk.Skipped {
		fmt.Fprintf(os.Stderr, "Skipped %s: %s\n", skip.Path, skip.Reason)
	}
	return nil
}

// unparsedWarning is the share of a file that has to be unreadable before the
// number is worth an explanation rather than just a column.
const unparsedWarning = 0.5

// warnUnparsed says what a large unparsed fraction probably means.
//
// The count on its own is honest but leaves the inference to the reader, and
// the inference is not obvious: a file can carry several formats, and until
// that occurs to you the number reads as "this tool cannot parse my logs".
//
// Judged per file rather than per row. A file read line by line has one row per
// format it turned out to contain, and the row holding what nothing claimed is
// 100% unparsed by construction — warning on that would fire on every mixed
// file however well it was read.
func warnUnparsed(w io.Writer, infos []store.SourceInfo) {
	type totals struct {
		records, unparsed int64
		formats           []string
	}

	var order []string
	byFile := map[string]*totals{}

	for _, si := range infos {
		t := byFile[si.File]
		if t == nil {
			t = &totals{}
			byFile[si.File] = t
			order = append(order, si.File)
		}
		t.records += si.Records
		t.unparsed += si.Unparsed
		t.formats = append(t.formats, si.Format)
	}

	for _, file := range order {
		t := byFile[file]
		if t.records == 0 || float64(t.unparsed)/float64(t.records) < unparsedWarning {
			continue
		}

		fmt.Fprintf(w, "\n%.1f%% of %s did not match %s.\n",
			100*float64(t.unparsed)/float64(t.records), file, describeFormats(t.formats))
		fmt.Fprintln(w, "Those records are still loaded and still searchable — "+
			"`parsed:false` lists them, and `loupe patterns` groups them by shape.")
	}
}

// describeFormats names what the file was read as.
func describeFormats(formats []string) string {
	for _, f := range formats {
		if f == parse.MixedName {
			return "any known format, even read line by line"
		}
	}
	if len(formats) == 1 {
		return fmt.Sprintf("the detected format %s", formats[0])
	}
	return "any of the formats detected in it"
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
