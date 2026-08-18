package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/GrantPukka/loupe/internal/query"
	"github.com/GrantPukka/loupe/internal/render"
	"github.com/GrantPukka/loupe/internal/session"
	"github.com/spf13/cobra"
)

func newPatternsCommand(g *globals) *cobra.Command {
	var (
		limit    int
		newSince string
		all      bool
	)

	cmd := &cobra.Command{
		Use:     "patterns [directory] [filter]",
		Aliases: []string{"pattern"},
		Short:   "Group messages into templates and count them",
		Long: `Collapse messages that differ only in their values into one template.

Thirty-four thousand lines are not thirty-four thousand different things. They
are a dozen shapes with the variable parts filled in differently, and seeing
them that way is what makes one anomaly visible in a wall of noise.

Only value-shaped tokens are masked — numbers, uuids, addresses, quoted
strings, paths, timestamps, hex ids. A bare word is never touched, so two
messages that differ by a word stay two templates. Erring toward too many
templates is deliberate: splitting one event in two is untidy, whereas merging
two different errors hides one of them.

Each template carries an id. Pass it back as a filter to see the records behind
it.`,
		Example: `  loupe patterns ./logs
  loupe patterns ./logs 'level:>=error'
  loupe patterns ./logs --new-since 15m
  loupe patterns ./logs --all`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPatterns(cmd, g, args, limit, newSince, all)
		},
	}

	cmd.Flags().IntVar(&limit, "limit", session.DefaultPatternLimit,
		"how many templates to show, most frequent first")
	cmd.Flags().BoolVar(&all, "all", false, "show every template, however long the tail")
	cmd.Flags().StringVar(&newSince, "new-since", "",
		"show only templates with no records older than this window, e.g. 15m")

	return cmd
}

func runPatterns(cmd *cobra.Command, g *globals, args []string, limit int, newSince string, all bool) error {
	given, filter, err := resolveArgs(args)
	if err != nil {
		return err
	}
	paths, _ := resolvePaths(g, given)

	var since time.Duration
	if newSince != "" {
		// The DSL's own duration parser, so --new-since 15m accepts exactly
		// what last:15m does and cannot drift from it.
		since, err = query.ParseDuration(newSince)
		if err != nil {
			return fmt.Errorf("--new-since %q: %w (try 15m, 2h, or 1d)", newSince, err)
		}
	}

	sess, err := g.open(cmd.Context(), paths...)
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

	set, err := sess.Patterns(cmd.Context(), plan, session.PatternQuery{
		Limit:    limit,
		NewSince: since,
	})
	if err != nil {
		return err
	}

	writePatterns(os.Stdout, os.Stderr, set, sess.Loc, g.quiet)
	return nil
}

// writePatterns renders a listing.
//
// The template is the wide column and goes last, because it is the only one
// with no natural width and the eye scans the counts down the left.
func writePatterns(out, status io.Writer, set session.PatternSet, loc *time.Location, quiet bool) {
	if !quiet {
		fmt.Fprintln(status)
	}

	if set.Templates == 0 {
		fmt.Fprintln(status, "No records matched, so there are no templates.")
		return
	}

	// The cutoff is stated in both zones before any result, like every other
	// window this tool prints, so nobody has to do offset arithmetic to know
	// what "new" meant.
	if !set.Since.IsZero() {
		fmt.Fprintf(status, "New since %s (%s), counted back from %s.\n",
			set.Since.In(loc).Format("2006-01-02 15:04:05 MST"),
			set.Since.UTC().Format("2006-01-02 15:04:05 UTC"),
			set.Anchor)
	}

	if len(set.Patterns) == 0 {
		fmt.Fprintf(status, "No new templates: all %s were already present before the cutoff.\n",
			countOf(set.Established, "template", "templates"))
		reportUndated(status, set)
		return
	}

	width := countWidth(set.Patterns)
	for _, p := range set.Patterns {
		fmt.Fprintf(out, "%s  %*s  %s\n", p.ID, width, render.Commas(p.Count), oneLine(p.Template))
	}

	writePatternFooter(status, set, loc)
}

func writePatternFooter(w io.Writer, set session.PatternSet, loc *time.Location) {
	fmt.Fprintf(w, "\n%s covering %s.\n",
		countOf(set.Templates, "template", "templates"),
		countOf(set.Records, "record", "records"))

	// Never a bare stop. What is not on screen is counted and named, or the
	// list quietly understates the data.
	if set.Truncated {
		fmt.Fprintf(w, "%s more not shown, covering %s — use --all.\n",
			countOf(set.Hidden, "template", "templates"),
			countOf(set.HiddenRecords, "record", "records"))
	}
	if set.Established > 0 {
		fmt.Fprintf(w, "%s already present before the cutoff, not shown.\n",
			countOf(set.Established, "template", "templates"))
	}

	reportUnparsed(w, set)
	reportUndated(w, set)

	if len(set.Patterns) > 0 {
		fmt.Fprintf(w, "Expand one with: loupe <dir> 'pattern:%s'\n", set.Patterns[0].ID)
	}
}

// reportUnparsed separates the templates that came from broken lines.
//
// Parsed messages collapse hard; a line no parser understood is genuinely its
// own shape, so a handful of truncated records can outnumber every real
// template. Without this the headline count reads as the collapse rule having
// failed, when it is the data saying something true.
func reportUnparsed(w io.Writer, set session.PatternSet) {
	if set.UnparsedTemplates == 0 {
		return
	}

	fmt.Fprintf(w, "%s of those templates come from %s no parser understood, where a "+
		"broken line is its own shape — add parsed:true to exclude them.\n",
		render.Commas(set.UnparsedTemplates),
		countOf(set.UnparsedRecords, "record", "records"))
}

// reportUndated states the records that cannot be placed in time.
//
// A record with no timestamp sits on neither side of a --new-since cutoff, so
// a template made only of them can never be shown to be new. Saying so beats
// letting the answer look complete.
func reportUndated(w io.Writer, set session.PatternSet) {
	if set.Undated == 0 {
		return
	}
	if set.Since.IsZero() {
		fmt.Fprintf(w, "%s %s no timestamp (ts:none).\n",
			countOf(set.Undated, "record", "records"), have(set.Undated))
		return
	}
	fmt.Fprintf(w, "%s %s no timestamp and could not be placed either side of the cutoff (ts:none).\n",
		countOf(set.Undated, "record", "records"), have(set.Undated))
}

// countOf renders a number with its noun, e.g. "1,234 templates".
//
// plural returns only the word — every other caller in this package supplies
// the number itself — and a footer built from the word alone reads "templates
// covering records", which says nothing at all.
func countOf(n int64, one, many string) string {
	return render.Commas(n) + " " + plural(int(n), one, many)
}

// have agrees the verb with its subject. "1 record have no timestamp" is the
// kind of small wrongness that makes a tool feel unfinished.
func have(n int64) string {
	if n == 1 {
		return "has"
	}
	return "have"
}

// countWidth right-aligns the counts so they read as a column.
func countWidth(patterns []session.Pattern) int {
	width := 0
	for _, p := range patterns {
		if n := len(render.Commas(p.Count)); n > width {
			width = n
		}
	}
	return width
}

// oneLine keeps a template on its own row.
//
// A template is built from the first line of a message, so this is defensive
// rather than expected — but a stray newline reaching here would break the
// column alignment of every row after it.
func oneLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}
