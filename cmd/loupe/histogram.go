package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/GrantPukka/loupe/internal/render"
	"github.com/GrantPukka/loupe/internal/session"
	"github.com/spf13/cobra"
)

func newHistogramCommand(g *globals) *cobra.Command {
	var buckets int

	cmd := &cobra.Command{
		Use:     "histogram [directory] [filter]",
		Aliases: []string{"hist"},
		Short:   "Show record counts over time",
		Long: `Draw the shape of the data over time, coloured by severity.

This is the timeline the web UI draws, in the terminal. Finding an error
cluster and then narrowing to it is the point: the window each bar covers is
printed so it can be typed straight back as a filter.`,
		Example: `  loupe histogram ./logs
  loupe histogram ./logs 'level:>=error'
  loupe histogram ./logs 'on:2026-08-13' --buckets 24`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHistogram(cmd, g, args, buckets)
		},
	}

	cmd.Flags().IntVar(&buckets, "buckets", session.DefaultBuckets,
		"how many intervals to divide the window into")

	return cmd
}

func runHistogram(cmd *cobra.Command, g *globals, args []string, buckets int) error {
	given, filter, err := resolveArgs(args)
	if err != nil {
		return err
	}
	paths, _ := resolvePaths(g, given)

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
		fmt.Fprintln(os.Stderr)
	}

	hist, err := sess.Histogram(cmd.Context(), plan, session.HistogramQuery{Buckets: buckets})
	if err != nil {
		return err
	}

	if len(hist.Buckets) == 0 {
		fmt.Fprintln(os.Stderr, "No records with a timestamp to plot.")
		if hist.NoTimestamp > 0 {
			fmt.Fprintf(os.Stderr, "%d matching record(s) have no timestamp — use ts:none to see them.\n",
				hist.NoTimestamp)
		}
		return nil
	}

	colour := !g.noColour && os.Getenv("NO_COLOR") == "" && render.IsTerminal(os.Stdout)
	drawHistogram(os.Stdout, hist, sess.Loc, colour)
	return nil
}

// blocks are the eighth-height glyphs used to draw a bar, so a bar can express
// a value more finely than a whole character.
var blocks = []rune{' ', '▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// drawHistogram writes one row per bucket.
//
// A row per bucket rather than a single sparkline, because the point is to find
// a cluster and then filter to it, and that needs the window of each bar
// visible and copyable.
func drawHistogram(w *os.File, hist session.Histogram, loc *time.Location, colour bool) {
	const width = 40

	// A shared scale across rows; a per-row scale would make every bucket look
	// equally busy and hide the cluster entirely.
	scale := float64(hist.Max)
	if scale == 0 {
		scale = 1
	}

	layout := "15:04:05"
	if hist.Interval >= 24*time.Hour {
		layout = "2006-01-02"
	} else if hist.Interval >= time.Minute {
		layout = "15:04"
	}

	for _, b := range hist.Buckets {
		bar := bar(float64(b.Count)/scale, width)
		if colour {
			bar = colourFor(b) + bar + "\x1b[0m"
		}

		fmt.Fprintf(w, "%s  %s %*d\n",
			b.Start.In(loc).Format(layout), bar, digits(hist.Max), b.Count)
	}

	fmt.Fprintf(w, "\n%d records in %d buckets of %s, peak %d\n",
		hist.Total, len(hist.Buckets), humanInterval(hist.Interval), hist.Max)

	if hist.NoTimestamp > 0 {
		fmt.Fprintf(w, "%d matching record(s) are not on the timeline: they have no timestamp (ts:none)\n",
			hist.NoTimestamp)
	}
}

// bar renders a fraction as a run of block characters.
func bar(fraction float64, width int) string {
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}

	eighths := int(fraction * float64(width) * 8)
	full, remainder := eighths/8, eighths%8

	var sb strings.Builder
	sb.WriteString(strings.Repeat(string(blocks[8]), full))
	if remainder > 0 {
		sb.WriteRune(blocks[remainder])
		full++
	}
	// A non-zero count must draw something, or a quiet period and a single
	// error look identical.
	if full == 0 && fraction > 0 {
		sb.WriteRune(blocks[1])
		full = 1
	}
	sb.WriteString(strings.Repeat(" ", max(0, width-full)))

	return sb.String()
}

// colourFor picks a bucket's colour from the most severe level in it.
//
// The whole reason to colour a timeline is that a red cluster is visible
// without reading any numbers, so the worst level wins rather than the most
// common one.
func colourFor(b session.Bucket) string {
	switch {
	case b.Levels["fatal"] > 0:
		return "\x1b[1m\x1b[31m"
	case b.Levels["error"] > 0:
		return "\x1b[31m"
	case b.Levels["warn"] > 0:
		return "\x1b[33m"
	case b.Levels["info"] > 0:
		return "\x1b[34m"
	default:
		return "\x1b[90m"
	}
}

func digits(n int64) int {
	d := 1
	for n >= 10 {
		n /= 10
		d++
	}
	return d
}

func humanInterval(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d >= time.Second:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	default:
		return d.String()
	}
}
