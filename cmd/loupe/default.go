package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/GrantPukka/loupe/internal/session"
	"github.com/GrantPukka/loupe/internal/workspace"
	"github.com/spf13/cobra"
)

// runDefault handles `loupe`, `loupe ./logs`, and `loupe ./logs '<filter>'`.
//
// Argument order is forgiving on purpose: under time pressure people type the
// filter first about as often as the path.
func runDefault(cmd *cobra.Command, g *globals, args []string) error {
	given, filter, err := resolveArgs(args)
	if err != nil {
		return err
	}
	paths, note := resolvePaths(g, given)

	// `loupe ./logs --ui` is the README's headline invocation and is exactly
	// `loupe serve ./logs`, so it runs the same code rather than a parallel one.
	if g.ui {
		if filter != "" {
			return fmt.Errorf("--ui takes a directory, not a filter — "+
				"type %q into the filter box once it opens", filter)
		}
		return runServe(cmd, g, given, g.uiAddr, false, true)
	}

	order, err := g.parseSort()
	if err != nil {
		return err
	}

	sess, err := g.open(cmd.Context(), paths...)
	if err != nil {
		return err
	}
	defer sess.Close()

	if !g.quiet {
		if note != "" {
			fmt.Fprintf(os.Stderr, "%s\n", note)
		}
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

	// A handoff is generated from this same plan. It must never be possible
	// for the exported records to differ from what was on screen.
	if g.handoff != "" {
		return runHandoff(cmd, g, sess, plan)
	}

	res, err := sess.Records(cmd.Context(), plan, session.RecordQuery{Limit: g.limit, Sort: order})
	if err != nil {
		return err
	}

	writer, err := g.renderer(sess.Loc)
	if err != nil {
		return err
	}
	if err := writer.Result(res); err != nil {
		return err
	}

	// An empty result is where this tool most easily misleads, so explain it
	// rather than leaving the user to guess whether their filter was wrong or
	// their data genuinely contains nothing.
	if res.RowCount() == 0 && !plan.Query.IsEmpty() {
		fmt.Fprintf(os.Stderr, "\n%s\n", sess.Explain(cmd.Context(), plan).Text)
	}
	return nil
}

// resolveArgs separates paths from filter terms.
//
// A path is something that exists on disk. Anything else is a filter, so
// `loupe 'level:error'` in a log directory does what it looks like it should,
// and several directories can be given at once to read them on one timeline.
//
// An argument shaped like a location but absent from disk is an error, not a
// filter term. Demoting it quietly means one typo reads the subscribed
// locations instead and answers a question about data nobody named.
func resolveArgs(args []string) (paths []string, filter string, err error) {
	for _, arg := range args {
		if _, statErr := os.Stat(arg); statErr == nil {
			paths = append(paths, arg)
			continue
		}
		if looksLikePath(arg) {
			return nil, "", fmt.Errorf("%s: no such file or directory", arg)
		}
		if filter == "" {
			filter = arg
		} else {
			filter += " " + arg
		}
	}
	return paths, filter, nil
}

// looksLikePath reports whether an argument was meant as a location rather than
// a filter term.
//
// A field term carries its colon before any slash, which is what separates
// `path:/api/checkout` from `logs/api`. A phrase carries whitespace, which is
// what separates `"GET /api/orders"` from `/var/log`. Anything genuinely on
// disk has already been matched by os.Stat before this is consulted, so the
// only job here is to classify what is missing.
func looksLikePath(arg string) bool {
	if arg == "" || strings.ContainsAny(arg, " \t") || strings.HasPrefix(arg, `"`) {
		return false
	}
	switch {
	case arg == ".", arg == "..":
		return true
	case strings.HasPrefix(arg, "./"), strings.HasPrefix(arg, "../"),
		strings.HasPrefix(arg, "/"), strings.HasPrefix(arg, "~"):
		return true
	}
	slash := strings.IndexByte(arg, '/')
	if slash < 0 {
		return false
	}
	colon := strings.IndexByte(arg, ':')
	return colon < 0 || colon > slash
}

// resolvePaths decides what to read when no directory was named.
//
// Subscriptions first, then the working directory. Someone who has subscribed
// to /var/log expects a bare `loupe` to read it, not whatever they happen to be
// standing in.
func resolvePaths(g *globals, given []string) ([]string, string) {
	if len(given) > 0 {
		return given, ""
	}

	work, err := workspace.Load(g.configDir)
	if err != nil {
		return []string{"."}, fmt.Sprintf("subscriptions unavailable (%v), reading .", err)
	}

	if active := work.ActivePaths(); len(active) > 0 {
		return active, fmt.Sprintf("reading %d subscribed location(s); "+
			"name a directory to read something else", len(active))
	}
	return []string{"."}, ""
}
