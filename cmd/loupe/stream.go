package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/GrantPukka/loupe/internal/render"
	"github.com/GrantPukka/loupe/internal/session"
	"github.com/GrantPukka/loupe/internal/store"
	"github.com/spf13/cobra"
)

// runStream prints matching records as they are read from a stream.
//
// The alternative — read to EOF, then query — is what the ordinary path does,
// and it is exactly wrong here: `kubectl logs -f api | loupe` never reaches
// EOF, so nothing would ever be printed for as long as the pod lived. To
// somebody watching a terminal that is a hang, not a feature.
//
// Records go through the store and the same compiled filter as everything
// else, rather than being matched in Go on the way past. A live view that
// filtered differently from the same query run afterwards would disagree with
// itself, which is worse than not having one.
func runStream(cmd *cobra.Command, g *globals, sess *session.Session, filter string) error {
	if g.handoff != "" {
		return errors.New("--handoff needs a finished read, and a stream has no end — " +
			"redirect it to a file first, then hand off from that")
	}

	writer, err := g.rendererFor(sess.Loc, true)
	if err != nil {
		return err
	}

	if !g.quiet {
		fmt.Fprintf(os.Stderr, "%s\n", session.StreamNote)
		fmt.Fprintln(os.Stderr, "Reading the stream. Records appear as they arrive; Ctrl-C to stop.")
		fmt.Fprintln(os.Stderr)
	}

	// Ctrl-C ends a stream the way closing the pipe does, so it exits zero
	// rather than reporting the interruption as a failure.
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var shown int64
	err = sess.Stream(ctx, filter, func(res store.Result) error {
		shown += int64(res.RowCount())
		return writer.Result(res)
	})

	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	if !g.quiet {
		summariseStream(sess, shown)
	}
	return nil
}

// summariseStream reports what the stream turned out to contain.
//
// A stream has no total to state up front — nobody knows how long a pipe is —
// so the counts that the status line prints before a file read are printed
// after a stream instead. They are still owed: a run that quietly read 900
// unparsed lines should say so.
func summariseStream(sess *session.Session, shown int64) {
	stats := sess.Load.Stats

	fmt.Fprintf(os.Stderr, "\n%s shown of %s read",
		render.Commas(shown), render.Commas(stats.Records))

	if stats.Unparsed > 0 {
		fmt.Fprintf(os.Stderr, " · %s unparsed", render.Commas(stats.Unparsed))
	}
	if stats.NoTimestamp > 0 {
		fmt.Fprintf(os.Stderr, " · %s without a timestamp", render.Commas(stats.NoTimestamp))
	}
	fmt.Fprintln(os.Stderr)
}
