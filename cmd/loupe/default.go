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
	paths := resolvePaths(g, given)

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

	if err := warnIfReadingATerminal(g, paths); err != nil {
		return err
	}

	// A summary cannot be true of records that have not arrived, so an
	// aggregation reads a stream to the end first, exactly as `loupe top` does.
	open := g.open
	if session.IsAggregate(filter) {
		open = g.openBatch
	}

	sess, err := open(cmd.Context(), paths...)
	if err != nil {
		return err
	}
	defer sess.Close()

	if g.follow && sess.HasStream() {
		return fmt.Errorf("--follow has nothing to poll on a stream: " +
			"standard input is already live, and ends when the writer closes it")
	}

	if !g.quiet {
		statusLine(os.Stderr, sess)
	}

	// A stream is read as it arrives. Everything below needs a finished ingest
	// to query, and a pipe from a running pod never finishes one.
	if sess.Streaming() {
		return runStream(cmd, g, sess, filter)
	}

	// PlanAggregate rather than Plan: this is the one command that can render
	// a `stats` clause, and every other caller of Plan refuses one rather than
	// dropping it silently.
	plan, err := sess.PlanAggregate(cmd.Context(), filter)
	if err != nil {
		return err
	}

	if !g.quiet {
		timeBanner(os.Stderr, sess, plan)
	}

	if plan.Query.Stats != nil {
		return runStats(cmd, g, sess, plan)
	}

	if !g.quiet {
		fmt.Fprintln(os.Stderr)
	}

	// A handoff is generated from this same plan. It must never be possible
	// for the exported records to differ from what was on screen.
	if g.handoff != "" {
		return runHandoff(cmd, g, sess, plan)
	}

	// The initial page is printed first either way: following starts from what
	// is already on screen, so the user sees context before the live tail.
	if g.context < 0 {
		return fmt.Errorf("--context takes a count of records, not %d", g.context)
	}
	if g.context > 0 && plan.Query.IsEmpty() {
		return fmt.Errorf("--context shows the records around a match; this filter matches everything\n" +
			"give a filter, or drop --context to list the lot")
	}

	if g.fold && g.context > 0 {
		return fmt.Errorf("--fold and --context ask for opposite things: " +
			"one hides repeated lines, the other shows more lines around them")
	}

	if g.format == "raw" && g.fold {
		return fmt.Errorf("--fold and --format raw ask for opposite things: " +
			"a folded row is a run of records, and there is no single original line to print")
	}

	res, err := sess.Records(cmd.Context(), plan, session.RecordQuery{
		Limit:   g.limit,
		Sort:    order,
		Context: g.context,
		Fold:    g.fold,
		Columns: rawColumnsFor(g.format, g.context),
	})
	if err != nil {
		return err
	}

	writer, err := g.renderer(sess.Loc)
	if err != nil {
		return err
	}
	if g.fold {
		// A folded row is a run of records, not one record, and a footer saying
		// "showing 6 of 32,371 records" would misstate both what is on screen
		// and the size of what the limit cut off.
		writer.SetRowNoun("run", "runs")
	}
	if err := writer.Result(res); err != nil {
		return err
	}

	// An empty result is where this tool most easily misleads, so explain it
	// rather than leaving the user to guess whether their filter was wrong or
	// their data genuinely contains nothing.
	if res.RowCount() == 0 && !plan.Query.IsEmpty() && !g.follow {
		fmt.Fprintf(os.Stderr, "\n%s\n", sess.Explain(cmd.Context(), plan).Text)
		if hint := subcommandHint(cmd, given, filter); hint != "" {
			fmt.Fprintf(os.Stderr, "%s\n", hint)
		}
	}

	if g.follow {
		return runFollow(cmd.Context(), g, sess, plan)
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
		// A bare dash is standard input, as it is for every other tool on the
		// system. It is checked before os.Stat because nothing on disk is
		// called "-", and before the filter fallback because otherwise it
		// would be read as a free-text search term for the word "-".
		if arg == session.StdinPath {
			paths = append(paths, arg)
			continue
		}

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

// warnIfReadingATerminal says so when stdin was asked for by name but nothing
// is piping into it.
//
// `loupe -` at a prompt is a legitimate request to read what gets typed, and
// refusing it would be wrong. Saying nothing would be worse: the tool sits
// there producing no output, which is indistinguishable from a hang in ingest,
// and the reason it is waiting is invisible.
func warnIfReadingATerminal(g *globals, paths []string) error {
	if g.quiet || stdinIsPiped() {
		return nil
	}
	for _, p := range paths {
		if p == session.StdinPath {
			fmt.Fprintln(os.Stderr,
				"Reading standard input from the terminal. Type or paste log lines, "+
					"then press Ctrl-D to finish, or Ctrl-C to stop.")
			return nil
		}
	}
	return nil
}

// stdinIsPiped reports whether standard input has data coming rather than a
// person sitting at a terminal.
//
// The distinction matters more than it looks. Reading a terminal nobody is
// typing into makes the tool hang with no output and no explanation, which is
// indistinguishable from a hang in ingest — so anything uncertain is treated
// as a terminal and left alone.
//
// A character device is a terminal or /dev/null, and neither is a log stream.
// `loupe < /dev/null` therefore reads the working directory, which is what it
// did before this existed.
func stdinIsPiped() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice == 0
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

// resolvePaths decides what to read when no directory was named, and says so.
//
// Subscriptions first, then the working directory. Someone who has subscribed
// to /var/log expects a bare `loupe` to read it, not whatever they happen to be
// standing in.
//
// It prints the explanation itself rather than returning it for the caller to
// print. Returned, it was dropped with `_` by five of the seven commands that
// call this — so `loupe top corpus service` mistyped its arguments, fell back
// to a subscription made eleven days earlier, and answered from an unrelated
// directory without ever naming it. Falling back is the documented feature;
// doing it in silence is not, and a report that cannot be silently discarded is
// the only version of it that stays true.
func resolvePaths(g *globals, given []string) []string {
	if len(given) > 0 {
		return given
	}

	// A pipe is something the user did deliberately, a moment ago. It outranks
	// subscriptions, which are ambient, and the working directory, which is an
	// accident of where the shell happens to be.
	if stdinIsPiped() {
		return notePaths(g, []string{session.StdinPath}, "reading standard input")
	}

	work, err := workspace.Load(g.configDir)
	if err != nil {
		return notePaths(g, []string{"."},
			fmt.Sprintf("subscriptions unavailable (%v), reading the working directory", err))
	}

	if active := work.Active(); len(active) > 0 {
		paths := make([]string, len(active))
		named := make([]string, len(active))
		for i, sub := range active {
			paths[i] = sub.Path
			named[i] = fmt.Sprintf("%s (subscription %q)", sub.Path, sub.Name())
		}
		// The paths, not the count. "reading 1 subscribed location(s)" is
		// exactly as much help as saying nothing when the question is whether
		// the answer came from the directory you meant.
		return notePaths(g, paths,
			"reading "+strings.Join(named, ", ")+"; name a directory to read something else")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return notePaths(g, []string{"."}, "reading the working directory")
	}
	return notePaths(g, []string{"."}, "reading "+cwd)
}

// notePaths prints why these paths were chosen and returns them unchanged.
func notePaths(g *globals, paths []string, note string) []string {
	if !g.quiet && note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	return paths
}

// rawColumnsFor selects the original text when the output format is raw.
//
// Empty for every other format, which leaves the listing on its default
// projection. --format raw exists to hand the file's own bytes to the next
// program in the pipe, and a listing that never selects raw could not do that
// on any filter query at all.
func rawColumnsFor(format string, context int) string {
	if format != "raw" {
		return ""
	}
	if context > 0 {
		return session.ContextRawColumns
	}
	return session.RawColumns
}

// subcommandHint spots a filter that is really a mistyped subcommand.
//
// `loupe corpus sources` is a search for the literal word "sources" and finds
// nothing, which is correct and unhelpful — the argument order for a subcommand
// is the other way round, and two of the findings in this project's bug list
// began as exactly this mistake. The hint only appears once the search has
// already come back empty, so someone genuinely grepping for the word never
// sees it.
func subcommandHint(cmd *cobra.Command, given []string, filter string) string {
	word := strings.TrimSpace(filter)
	if word == "" || strings.ContainsAny(word, " :\"") {
		return ""
	}

	root := cmd.Root()
	for _, sub := range root.Commands() {
		if sub.Name() != word {
			continue
		}
		where := strings.Join(given, " ")
		if where == "" {
			return fmt.Sprintf("`%s` is also a subcommand. Did you mean `loupe %s`?", word, word)
		}
		return fmt.Sprintf("`%s` is also a subcommand. Did you mean `loupe %s %s`?", word, word, where)
	}
	return ""
}
