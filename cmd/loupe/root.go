package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/GrantPukka/loupe/internal/render"
	"github.com/GrantPukka/loupe/internal/server"
	"github.com/GrantPukka/loupe/internal/session"
	"github.com/GrantPukka/loupe/internal/source"
	"github.com/spf13/cobra"
)

// defaultDisplayLimit is how many rows the terminal shows by default.
const defaultDisplayLimit = 200

// globals holds the flags shared by every command that reads logs.
type globals struct {
	parser      string
	format      string
	limit       int
	noColour    bool
	utc         bool
	tz          string
	sourceTZ    []string
	maxFileSize int64
	include     []string
	exclude     []string
	quiet       bool
	follow      bool
	relativeTo  string
	noCache     bool
	cacheDir    string
	ui          bool
	uiAddr      string
	handoff     string
	redact      []string
	sort        string
	configDir   string
	context     int
	fold        bool
}

func newRootCommand() *cobra.Command {
	g := &globals{}

	root := &cobra.Command{
		Use:   "loupe [directory] [filter]",
		Short: "Explore a directory of mixed-format log files",
		Long: `loupe reads every log file in a directory, whatever formats they are in,
normalises them onto one timeline, and lets you filter it.

A filter that ends in a stats clause summarises instead of listing:

    loupe ./logs 'level:>=error stats count() by path'
    loupe ./logs 'last:1h stats count(), p99(latency_ms) by source, bin(1m)'

Read-only, local-only, no daemon, no network.`,
		Version:      version,
		SilenceUsage: true,
		// main prints the error. Leaving this false makes cobra print it too,
		// so every failure appears twice.
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDefault(cmd, g, args)
		},
	}

	pf := root.PersistentFlags()
	pf.StringVar(&g.parser, "parser", "", "force a log format instead of detecting it")
	pf.StringVar(&g.format, "format", "",
		"output format: table, json, ndjson, raw, csv (default table on a terminal, ndjson when piped)")
	pf.IntVar(&g.limit, "limit", defaultDisplayLimit, "maximum rows to display (0 for no limit)")
	pf.BoolVar(&g.noColour, "no-color", false, "disable colour output")
	pf.BoolVar(&g.utc, "utc", false, "show times in UTC")
	pf.StringVar(&g.tz, "tz", "", "display timezone, e.g. Europe/London")
	pf.StringSliceVar(&g.sourceTZ, "source-tz", nil,
		"timezone assumed for sources whose format carries none, e.g. UTC or postgres:Europe/London")
	pf.Int64Var(&g.maxFileSize, "max-file-size", 0, "skip files larger than this many bytes")
	pf.StringSliceVar(&g.include, "include", nil, "only read files matching these globs")
	pf.StringSliceVar(&g.exclude, "exclude", nil, "skip files matching these globs")
	pf.BoolVarP(&g.quiet, "quiet", "q", false, "suppress the status line")
	pf.StringVar(&g.relativeTo, "relative-to", "newest",
		"what last: counts back from: newest (the newest record) or now (the wall clock)")
	pf.BoolVar(&g.noCache, "no-cache", false, "re-read the log files instead of reusing a cached ingest")
	pf.StringVar(&g.cacheDir, "cache-dir", "", "override the cache location (default ~/.cache/loupe)")
	pf.StringVar(&g.handoff, "handoff", "",
		"write a pasteable extract to this file instead of printing records (.md, .json, .zip, or - for stdout)")
	pf.StringSliceVar(&g.redact, "redact", nil,
		"replace these field values with a stable hash in the handoff, e.g. --redact user_id,email")
	pf.StringVar(&g.sort, "sort", "oldest", "record order: oldest or newest first")
	pf.StringVar(&g.configDir, "config-dir", "", "override where subscriptions live (default ~/.config/loupe)")

	// The README's headline invocation. It is the same thing `loupe serve`
	// does, so it delegates rather than growing a second code path.
	root.Flags().BoolVar(&g.follow, "follow", false,
		"keep watching for records written after the initial read")
	root.Flags().IntVarP(&g.context, "context", "C", 0,
		"also show this many records either side of each match, from the same file")
	root.Flags().BoolVar(&g.fold, "fold", false,
		"collapse consecutive repeats of the same line into one row with a count")
	root.Flags().BoolVar(&g.ui, "ui", false, "open the results in a local web UI instead of printing them")
	root.Flags().StringVar(&g.uiAddr, "addr", server.DefaultAddr, "loopback address for --ui")

	root.AddCommand(
		newDemoCommand(g),
		newSubscribeCommand(g),
		newUnsubscribeCommand(g),
		newSubscriptionsCommand(g),
		newSQLCommand(g),
		newSourcesCommand(g),
		newCacheCommand(g),
		newHistogramCommand(g),
		newPatternsCommand(g),
		newTraceCommand(g),
		newTopCommand(g),
		newFieldsCommand(g),
		newDiffCommand(g),
		newServeCommand(g),
		newTUICommand(g),
	)

	return root
}

// open resolves the flags into session options and opens the logs.
func (g *globals) open(ctx context.Context, paths ...string) (*session.Session, error) {
	opts, err := g.sessionOptions(paths)
	if err != nil {
		return nil, err
	}

	sess, err := session.Open(ctx, opts)
	if err != nil {
		// A walk that found nothing has to say what it passed over. An empty
		// result with no explanation is the most misleading outcome possible.
		var none session.NoSourcesError
		if errorsAs(err, &none) {
			return nil, describeNoSources(none)
		}
		return nil, err
	}
	return sess, nil
}

func (g *globals) sessionOptions(paths []string) (session.Options, error) {
	loc, err := session.ParseLocation(g.utc, g.tz)
	if err != nil {
		return session.Options{}, err
	}

	zones, err := session.ParseSourceZones(g.sourceTZ)
	if err != nil {
		return session.Options{}, err
	}

	relativeToNow, err := g.parseRelativeTo()
	if err != nil {
		return session.Options{}, err
	}

	return session.Options{
		Paths:         paths,
		Parser:        g.parser,
		SourceZones:   zones,
		Location:      loc,
		RelativeToNow: relativeToNow,
		NoCache:       g.noCache,
		CacheDir:      g.cacheDir,
		Walk: source.WalkOptions{
			MaxFileSize: g.maxFileSize,
			Include:     g.include,
			Exclude:     g.exclude,
		},
	}, nil
}

// parseSort reads --sort.
//
// The terminal defaults to oldest first, which is what makes a cascade legible
// and what a piped `loupe … | head` expects. The web UI asks for newest first
// explicitly, because there the top of the list is where the eye starts.
func (g *globals) parseSort() (session.SortOrder, error) {
	switch strings.ToLower(g.sort) {
	case "", "oldest", "time", "asc":
		return session.SortTime, nil
	case "newest", "-time", "desc", "recent":
		return session.SortTimeDesc, nil
	default:
		return "", fmt.Errorf("unknown --sort %q: use oldest or newest", g.sort)
	}
}

// parseRelativeTo reads --relative-to.
//
// The default anchors last: to the newest record rather than the wall clock,
// because last:15m against a log file from yesterday returning nothing is the
// single most confusing possible result.
func (g *globals) parseRelativeTo() (bool, error) {
	switch strings.ToLower(g.relativeTo) {
	case "", "newest", "data":
		// docs/FILTER-DSL.md section 2.2: follow mode anchors to the wall clock
		// instead. The newest record is a moving target while records are
		// arriving, so last:15m would slide forward on every poll and quietly
		// drop the beginning of the window the user is watching.
		if g.follow && !g.explicitRelativeTo() {
			return true, nil
		}
		return false, nil
	case "now", "wall", "wallclock":
		return true, nil
	default:
		return false, fmt.Errorf("unknown --relative-to %q: use newest or now", g.relativeTo)
	}
}

// openBatch opens the logs for a command that cannot answer until the whole
// read has finished.
//
// Grouping into templates, drawing a timeline, or running SQL over the lot
// cannot say anything true about records that have not arrived yet, so a
// stream is read to the end first. That blocks until the writer closes the
// pipe, which is what `sort` or `wc` do with a pipe and what anyone typing
// this expects — but it is said out loud, because a tool sitting silently on
// a pipe is indistinguishable from a tool that has hung.
func (g *globals) openBatch(ctx context.Context, paths ...string) (*session.Session, error) {
	sess, err := g.open(ctx, paths...)
	if err != nil {
		return nil, err
	}

	if sess.Streaming() {
		if !g.quiet {
			fmt.Fprintln(os.Stderr,
				"Reading standard input to the end before answering; this waits for the pipe to close.")
		}
		if err := sess.Drain(ctx); err != nil {
			sess.Close()
			return nil, err
		}
	}
	return sess, nil
}

func (g *globals) renderer(loc *time.Location) (*render.Writer, error) {
	return g.rendererFor(loc, false)
}

// sqlRenderer builds a renderer for output whose columns the user named.
//
// See render.Options.UserSQL: a TIMESTAMP a user computed is not an instant,
// and must not be moved by the display offset.
func (g *globals) sqlRenderer(loc *time.Location) (*render.Writer, render.Options, error) {
	opts, err := g.renderOptions(loc, false)
	if err != nil {
		return nil, opts, err
	}
	opts.UserSQL = true
	return render.New(os.Stdout, opts), opts, nil
}

// rendererFor builds a renderer, marking it continuous for output that arrives
// in batches but is one listing — a live tail, or a stream being read as it is
// written.
func (g *globals) rendererFor(loc *time.Location, continuous bool) (*render.Writer, error) {
	opts, err := g.renderOptions(loc, continuous)
	if err != nil {
		return nil, err
	}
	return render.New(os.Stdout, opts), nil
}

// renderOptions resolves the flags into render options, for the callers that
// need to inspect them as well as render with them.
func (g *globals) renderOptions(loc *time.Location, continuous bool) (render.Options, error) {
	opts := render.Options{
		Location:   loc,
		Continuous: continuous,
		Colour:     !g.noColour && os.Getenv("NO_COLOR") == "" && render.IsTerminal(os.Stdout),
	}
	if g.format != "" {
		f, err := render.ParseFormat(g.format)
		if err != nil {
			return opts, err
		}
		opts.Format = f
	}
	if opts.Format == "" {
		// Resolved here rather than left to render.New, so that a caller
		// reading these options back sees the format that will actually be
		// used.
		if render.IsTerminal(os.Stdout) {
			opts.Format = render.FormatTable
		} else {
			opts.Format = render.FormatNDJSON
		}
	}
	return opts, nil
}

// describeNoSources expands the error with the list of skipped files.
func describeNoSources(none session.NoSourcesError) error {
	if len(none.Skipped) == 0 {
		return none
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "no readable log files in %s, but %d file(s) were skipped:",
		strings.Join(none.Paths, ", "), len(none.Skipped))

	const show = 10
	for i, s := range none.Skipped {
		if i == show {
			fmt.Fprintf(&sb, "\n  ... and %d more", len(none.Skipped)-show)
			break
		}
		fmt.Fprintf(&sb, "\n  %s: %s", s.Path, s.Reason)
	}
	return fmt.Errorf("%s", sb.String())
}

// errorsAs is errors.As for a concrete error value.
func errorsAs(err error, target *session.NoSourcesError) bool {
	if v, ok := err.(session.NoSourcesError); ok {
		*target = v
		return true
	}
	return false
}

// explicitRelativeTo reports whether the user named an anchor themselves.
//
// Follow mode changes the default, but never overrides a choice that was typed:
// --follow --relative-to=newest is a coherent thing to ask for.
func (g *globals) explicitRelativeTo() bool {
	switch strings.ToLower(g.relativeTo) {
	case "newest", "data", "now", "wall", "wallclock":
		return true
	default:
		return false
	}
}
