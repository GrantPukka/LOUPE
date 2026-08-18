package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/GrantPukka/loupe/internal/session"
	"github.com/spf13/cobra"
)

func newTraceCommand(g *globals) *cobra.Command {
	var field string

	cmd := &cobra.Command{
		Use:   "trace <id> [directory]",
		Short: "Follow one request across every source",
		Long: `Put one request's records on a single timeline, in the order they happened.

Watch the pool exhaust, then the app error, then the 502, with the wait between
each hop measured. The wait is usually the finding: five lines that all look
fine and one four-second gap between two of them.

The correlation field is detected — trace_id, traceId, request_id, req_id,
x-request-id, correlation_id — and the one chosen is printed, so a wrong guess
is visible rather than silent. Use --field to name it yourself.

Sources that carry no correlation id at all are listed separately from sources
that carry them but not this one. The first may well have handled the request
and cannot say; the second probably did not. Collapsing the two would invite
you to conclude a request skipped a service it went straight through.`,
		Example: `  loupe trace a91c40f2 ./logs
  loupe trace a91c40f2 ./logs --field request_id
  loupe trace a91c40f2 ./logs --handoff incident.md`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTrace(cmd, g, args, field)
		},
	}

	cmd.Flags().StringVar(&field, "field", "",
		"correlation field to follow, when detection picks the wrong one")

	return cmd
}

func runTrace(cmd *cobra.Command, g *globals, args []string, field string) error {
	id := args[0]

	given, filter, err := resolveArgs(args[1:])
	if err != nil {
		return err
	}
	if filter != "" {
		return fmt.Errorf("trace takes an id and a directory, not a filter — "+
			"a trace is already one request. Did you mean `loupe %s %q`?",
			strings.Join(given, " "), filter)
	}
	paths, _ := resolvePaths(g, given)

	// A trace is a whole-dataset question: it cannot know which sources stayed
	// silent until every source has been read.
	sess, err := g.openBatch(cmd.Context(), paths...)
	if err != nil {
		return err
	}
	defer sess.Close()

	if !g.quiet {
		statusLine(os.Stderr, sess)
	}

	trace, err := sess.Trace(cmd.Context(), id, field)
	if err != nil {
		return err
	}

	if g.handoff != "" {
		return runTraceHandoff(cmd, g, sess, trace)
	}

	writeTrace(os.Stdout, os.Stderr, trace, sess.Loc, g.quiet)
	return nil
}

// writeTrace renders a trace timeline.
//
// The hops go to stdout so the timeline can be piped; everything about how to
// read it goes to stderr, so a pipe cannot swallow the caveats.
func writeTrace(out, status io.Writer, t session.Trace, loc *time.Location, quiet bool) {
	if !quiet {
		fmt.Fprintln(status)
	}

	if !t.Found() {
		writeTraceNotFound(status, t)
		return
	}

	if !quiet {
		fmt.Fprintf(status, "Trace %s · %s · %s across %s\n",
			t.ID, t.Field,
			countOf(int64(len(t.Hops)), "hop", "hops"),
			countOf(int64(len(t.Present())), "source", "sources"))
	}

	slowest := t.Slowest()
	width := gapWidth(t)

	for i, h := range t.Hops {
		marker := "  "
		if i == slowest {
			// The largest wait is the reason to draw a timeline at all, so it
			// is the one thing marked.
			marker = "▸ "
		}

		fmt.Fprintf(out, "%s%s  %*s  %-14s %-5s %s\n",
			marker,
			hopClock(h, loc),
			width, hopGap(h),
			truncateField(h.Source, 14),
			h.Level,
			firstLine(h.Message))
	}

	writeTraceFooter(status, t, loc)
}

func writeTraceNotFound(w io.Writer, t session.Trace) {
	fmt.Fprintf(w, "No records carry %s %s.\n", t.Field, t.ID)

	if silent := t.Silent(); len(silent) > 0 {
		fmt.Fprintf(w, "%s do record %s, so this id is not one of theirs.\n",
			namesOf(silent), t.Field)
	}
	if blind := t.Blind(); len(blind) > 0 {
		fmt.Fprintf(w, "%s never record %s, so they could not be checked.\n",
			namesOf(blind), t.Field)
	}
}

func writeTraceFooter(w io.Writer, t session.Trace, loc *time.Location) {
	fmt.Fprintln(w)

	if t.Span > 0 {
		fmt.Fprintf(w, "Span %s", humanGap(t.Span))
		if at := t.Slowest(); at >= 0 {
			fmt.Fprintf(w, ", of which %s waiting before %s",
				humanGap(t.Hops[at].Gap), t.Hops[at].Source)
		}
		fmt.Fprintln(w, ".")
	}

	// A trace is a claim about where a request went, so what could not be
	// checked is stated with it. Listing only the sources that matched would
	// let a reader conclude the request skipped everything else.
	if blind := t.Blind(); len(blind) > 0 {
		fmt.Fprintf(w, "%s never record %s, so this trace cannot say whether the "+
			"request reached them.\n", namesOf(blind), t.Field)
	}
	if silent := t.Silent(); len(silent) > 0 {
		fmt.Fprintf(w, "%s record %s but none for this one.\n", namesOf(silent), t.Field)
	}

	// Never dropped, always counted: a record without a clock is still a
	// record, and on a crashed service it is often the last one.
	if t.Undated > 0 {
		fmt.Fprintf(w, "%s carry no timestamp and are listed last, in ingest order.\n",
			countOf(int64(t.Undated), "hop", "hops"))
	}

	if len(t.Hops) > 0 {
		fmt.Fprintf(w, "Every record: loupe <dir> '%s:%s'\n", t.Field, t.ID)
	}
}

// hopClock renders a hop's time, or says it has none.
func hopClock(h session.Hop, loc *time.Location) string {
	if !h.Dated() {
		return strings.Repeat(" ", len("15:04:05.000")-len("no timestamp")) + "no timestamp"
	}
	return h.Time.In(loc).Format("15:04:05.000")
}

// hopGap renders the wait before a hop.
func hopGap(h session.Hop) string {
	if !h.HasGap {
		return ""
	}
	return "+" + humanGap(h.Gap)
}

func gapWidth(t session.Trace) int {
	width := 0
	for _, h := range t.Hops {
		if n := len(hopGap(h)); n > width {
			width = n
		}
	}
	return width
}

// humanGap renders a duration at a precision a reader can compare at a glance.
//
// Full precision on a four-millisecond gap and on a four-second one makes the
// two hard to tell apart, which defeats the point of showing them side by side.
func humanGap(d time.Duration) string {
	switch {
	case d >= time.Minute:
		return d.Round(time.Second).String()
	case d >= time.Second:
		return d.Round(10 * time.Millisecond).String()
	case d >= time.Millisecond:
		return d.Round(time.Millisecond).String()
	default:
		return d.Round(time.Microsecond).String()
	}
}

func namesOf(reach []session.SourceReach) string {
	names := make([]string, len(reach))
	for i, r := range reach {
		names[i] = r.Name
	}
	return strings.Join(names, ", ")
}

func truncateField(s string, width int) string {
	if len(s) <= width {
		return s
	}
	return s[:width-1] + "…"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

// runTraceHandoff writes a trace as a pasteable extract. Built in EC003.2.
func runTraceHandoff(cmd *cobra.Command, g *globals, sess *session.Session, t session.Trace) error {
	return fmt.Errorf("--handoff for a trace is not built yet; "+
		"for now, `loupe <dir> '%s:%s' --handoff` exports the records without the timeline",
		t.Field, t.ID)
}
