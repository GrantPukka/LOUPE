package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/GrantPukka/loupe/internal/render"
	"github.com/GrantPukka/loupe/internal/session"
	"github.com/GrantPukka/loupe/internal/store"
)

// runFollow prints matching records as they are written, until interrupted.
//
// Records go through the store and the same compiled filter as everything else,
// rather than being matched in Go on the way past. A live view that filtered
// differently from the same query run afterwards would be worse than no live
// view: it would disagree with itself.
func runFollow(ctx context.Context, g *globals, sess *session.Session, plan session.Plan) error {
	follower, err := sess.Follower(ctx)
	if err != nil {
		return err
	}

	// Continuous: the tail is one listing arriving in pieces, so the table
	// header belongs at the top of it and not above every batch.
	writer, err := g.rendererFor(sess.Loc, true)
	if err != nil {
		return err
	}

	if !g.quiet {
		fmt.Fprintf(os.Stderr, "Following %d %s. Ctrl-C to stop.\n\n",
			len(sess.Paths), plural(len(sess.Paths), "location", "locations"))
	}

	ticker := time.NewTicker(store.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// An interrupted follow is the normal way to end one, not a
			// failure, so it exits zero.
			if !g.quiet {
				fmt.Fprintln(os.Stderr, "\nStopped following.")
			}
			return nil

		case <-ticker.C:
			if err := followOnce(ctx, g, sess, plan, follower, writer); err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				return err
			}
		}
	}
}

// followOnce polls, then prints whatever the poll made newly visible.
func followOnce(ctx context.Context, g *globals, sess *session.Session, plan session.Plan,
	follower *store.Follower, writer *render.Writer) error {

	batch, err := follower.Poll(ctx)
	if err != nil {
		return err
	}

	// A source that failed to read is reported and skipped. Ending the session
	// because one file became unreadable would take the other five with it.
	for _, e := range batch.Errors {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}

	if batch.Records == 0 {
		return nil
	}

	where, args := batch.Predicate()
	res, err := sess.Records(ctx, plan, session.RecordQuery{
		Sort:      session.SortTime,
		Where:     where,
		WhereArgs: args,
	})
	if err != nil {
		return err
	}
	if res.RowCount() == 0 {
		// Records arrived but none matched the filter. That is not silence
		// worth reporting on every tick; the status line already said what the
		// filter is.
		return nil
	}

	return writer.Result(res)
}
