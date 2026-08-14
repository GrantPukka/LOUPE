package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/VIGIL-OPS/loupe/internal/render"
	"github.com/VIGIL-OPS/loupe/internal/server"
	"github.com/VIGIL-OPS/loupe/internal/session"
	"github.com/VIGIL-OPS/loupe/internal/source"
	"github.com/spf13/cobra"
)

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
	relativeTo  string
	noCache     bool
	cacheDir    string
	ui          bool
	uiAddr      string
}

func newRootCommand() *cobra.Command {
	g := &globals{}

	root := &cobra.Command{
		Use:   "loupe [directory] [filter]",
		Short: "Explore a directory of mixed-format log files",
		Long: `loupe reads every log file in a directory, whatever formats they are in,
normalises them onto one timeline, and lets you filter it.

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
	pf.StringVar(&g.format, "format", "", "output format: table, json, ndjson, raw, csv")
	pf.IntVar(&g.limit, "limit", 200, "maximum rows to display (0 for no limit)")
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

	// The README's headline invocation. It is the same thing `loupe serve`
	// does, so it delegates rather than growing a second code path.
	root.Flags().BoolVar(&g.ui, "ui", false, "open the results in a local web UI instead of printing them")
	root.Flags().StringVar(&g.uiAddr, "addr", server.DefaultAddr, "loopback address for --ui")

	root.AddCommand(
		newSQLCommand(g),
		newSourcesCommand(g),
		newCacheCommand(g),
		newHistogramCommand(g),
		newServeCommand(g),
		newTUICommand(g),
	)

	return root
}

// open resolves the flags into session options and opens the logs.
func (g *globals) open(ctx context.Context, path string) (*session.Session, error) {
	opts, err := g.sessionOptions(path)
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

func (g *globals) sessionOptions(path string) (session.Options, error) {
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
		Path:          path,
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

// parseRelativeTo reads --relative-to.
//
// The default anchors last: to the newest record rather than the wall clock,
// because last:15m against a log file from yesterday returning nothing is the
// single most confusing possible result.
func (g *globals) parseRelativeTo() (bool, error) {
	switch strings.ToLower(g.relativeTo) {
	case "", "newest", "data":
		return false, nil
	case "now", "wall", "wallclock":
		return true, nil
	default:
		return false, fmt.Errorf("unknown --relative-to %q: use newest or now", g.relativeTo)
	}
}

func (g *globals) renderer(loc *time.Location) (*render.Writer, error) {
	opts := render.Options{
		Location: loc,
		Colour:   !g.noColour && os.Getenv("NO_COLOR") == "" && render.IsTerminal(os.Stdout),
	}
	if g.format != "" {
		f, err := render.ParseFormat(g.format)
		if err != nil {
			return nil, err
		}
		opts.Format = f
	}
	return render.New(os.Stdout, opts), nil
}

// describeNoSources expands the error with the list of skipped files.
func describeNoSources(none session.NoSourcesError) error {
	if len(none.Skipped) == 0 {
		return none
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "no readable log files in %s, but %d file(s) were skipped:",
		none.Path, len(none.Skipped))

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
