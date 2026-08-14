package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/VIGIL-OPS/loupe/internal/query"
	"github.com/VIGIL-OPS/loupe/internal/render"
	"github.com/VIGIL-OPS/loupe/internal/source"
	"github.com/VIGIL-OPS/loupe/internal/store"
	"github.com/spf13/cobra"
)

// globals holds the flags shared by every command that reads logs.
type globals struct {
	// path is the directory or file to read. Defaults to the current
	// directory, so `loupe` alone does something sensible.
	path string

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

	root.AddCommand(
		newSQLCommand(g),
		newSourcesCommand(g),
	)

	return root
}

// session is an opened set of logs, ready to query.
type session struct {
	db     *store.DB
	load   store.Load
	walk   *source.WalkOptions
	loc    *time.Location
	writer *render.Writer
	limit  int

	// schema is resolved lazily and cached: it costs two queries and both the
	// filter and the empty-result explanation need it.
	schema *query.Schema
}

func (s *session) Close() error { return s.db.Close() }

// open walks the path, ingests everything, and returns a queryable session.
func (g *globals) open(ctx context.Context, path string) (*session, error) {
	loc, err := g.location()
	if err != nil {
		return nil, err
	}

	walk := &source.WalkOptions{
		MaxFileSize: g.maxFileSize,
		Include:     g.include,
		Exclude:     g.exclude,
	}

	sources, err := source.Walk(path, walk)
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, noSourcesError(path, walk)
	}

	zones, err := parseSourceTZ(g.sourceTZ)
	if err != nil {
		return nil, err
	}

	// In-memory for now. The fingerprint cache arrives in a later milestone.
	db, err := store.Open("")
	if err != nil {
		return nil, err
	}

	load, err := db.Load(ctx, sources, store.LoadOptions{
		Parser:      g.parser,
		SourceZones: zones,
	})
	if err != nil {
		db.Close()
		return nil, err
	}

	writer, err := g.renderer(loc)
	if err != nil {
		db.Close()
		return nil, err
	}

	return &session{db: db, load: load, walk: walk, loc: loc, writer: writer, limit: g.limit}, nil
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

// location resolves the display timezone.
//
// One timezone for the whole session, defaulting to the system zone, and always
// stated on screen. A user must never have to guess whether the times they are
// reading are theirs or the server's.
func (g *globals) location() (*time.Location, error) {
	switch {
	case g.utc && g.tz != "":
		return nil, fmt.Errorf("--utc and --tz are mutually exclusive")
	case g.utc:
		return time.UTC, nil
	case g.tz != "":
		loc, err := time.LoadLocation(g.tz)
		if err != nil {
			return nil, fmt.Errorf("unknown timezone %q: try a name from the tz database, e.g. Europe/London or UTC", g.tz)
		}
		return loc, nil
	default:
		return systemLocation(), nil
	}
}

// systemLocation resolves the local zone to a named one.
//
// time.Local stringifies as "Local", which tells a user nothing and cannot be
// pasted into a ticket. The spec requires the active timezone be visible and
// unambiguous, so dig out the real name and only fall back to time.Local when
// the system will not say.
func systemLocation() *time.Location {
	if tz := os.Getenv("TZ"); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}

	// /etc/localtime is a symlink into the zoneinfo tree on Linux and macOS.
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(target, "zoneinfo/"); i >= 0 {
			name := target[i+len("zoneinfo/"):]
			if loc, err := time.LoadLocation(name); err == nil {
				return loc
			}
		}
	}

	return time.Local
}

// parseSourceTZ reads --source-tz values.
//
// Two shapes: a bare zone applying to every source, and source:zone naming one.
// Both may be given, and the named one wins.
func parseSourceTZ(values []string) (map[string]*time.Location, error) {
	if len(values) == 0 {
		return nil, nil
	}

	out := map[string]*time.Location{}
	for _, v := range values {
		name, zone := "", v
		if i := strings.LastIndex(v, ":"); i > 0 {
			name, zone = v[:i], v[i+1:]
		}

		loc, err := time.LoadLocation(zone)
		if err != nil {
			return nil, fmt.Errorf("unknown timezone %q in --source-tz %q: "+
				"use a tz database name, e.g. --source-tz=UTC or --source-tz=postgres:Europe/London", zone, v)
		}
		out[name] = loc
	}
	return out, nil
}

// noSourcesError explains why nothing was read, listing what was skipped.
//
// An empty result with no explanation is the single most misleading thing this
// tool could do, so a walk that found nothing has to say what it passed over.
func noSourcesError(path string, walk *source.WalkOptions) error {
	if len(walk.Skipped) == 0 {
		return fmt.Errorf("no log files found in %s", path)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "no readable log files in %s, but %d file(s) were skipped:",
		path, len(walk.Skipped))

	const show = 10
	for i, s := range walk.Skipped {
		if i == show {
			fmt.Fprintf(&sb, "\n  ... and %d more", len(walk.Skipped)-show)
			break
		}
		fmt.Fprintf(&sb, "\n  %s: %s", s.Path, s.Reason)
	}
	return fmt.Errorf("%s", sb.String())
}
