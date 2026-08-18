package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/GrantPukka/loupe/internal/query"
	"github.com/GrantPukka/loupe/internal/schema"
	"github.com/GrantPukka/loupe/internal/source"
	"github.com/GrantPukka/loupe/internal/store"
)

// StreamNote explains what a streaming read gives up, for the status line.
//
// Promotion rewrites the logs table, and rewriting a table that an appender is
// still writing into is not something to attempt halfway through a stream. The
// consequence is a performance one rather than a correctness one — every field
// is still queryable, read out of the JSON bag instead of a column — but it is
// a difference from reading the same data as a file, so it is stated.
const StreamNote = "streaming: fields are read from the JSON bag, not promoted to columns"

// Emit receives one batch of newly matching records.
type Emit func(store.Result) error

// Stream reads the pending sources, emitting matching records as they arrive.
//
// This is what makes `kubectl logs -f api | loupe` work. A pipe from a running
// pod never reaches EOF, so the ordinary path — read everything, then query —
// would sit there producing nothing for as long as the pod lived. To the person
// watching that is indistinguishable from a hang, and it is the failure this
// whole item exists to remove.
//
// The filter is compiled once, against the first batch to arrive. A field that
// only shows up later is therefore not known when the filter is resolved: that
// is an error rather than an empty result, for the same reason a typo'd field
// name is, and the message says which records the filter was resolved against.
func (s *Session) Stream(ctx context.Context, filter string, emit Emit) error {
	if len(s.pending) == 0 {
		return errors.New("nothing to stream: this session was not opened for streaming")
	}

	var (
		plan     Plan
		havePlan bool
		// streamErr is what a batch failed with. Load collects a source's
		// error rather than returning it, so a cancelled or failed stream
		// would otherwise be reported as a clean finish.
		streamErr error
		// mark is the sequence number the next emitted batch starts at. It
		// trails the ingest until the filter has been resolved, so records
		// that arrived before then are not skipped.
		mark int64
	)

	opts := s.load
	opts.OnBatch = func(int64) error {
		if streamErr != nil {
			return streamErr
		}
		if err := ctx.Err(); err != nil {
			streamErr = err
			return err
		}

		if !havePlan {
			// Resolving needs to know which fields exist, and nothing did
			// until this batch landed. The cached schema is dropped first
			// because it was computed over an empty table.
			s.invalidateSchema()

			p, err := s.Plan(ctx, filter)
			if err != nil {
				streamErr = streamPlanError(err)
				return streamErr
			}
			plan, havePlan = p, true
			// Everything so far, not just this batch: earlier records arrived
			// before there was a filter to test them against.
			mark = 0
		}

		res, err := s.Records(ctx, plan, RecordQuery{
			Sort:      SortTime,
			Where:     "seq >= ?",
			WhereArgs: []any{mark},
		})
		if err != nil {
			streamErr = err
			return err
		}

		mark = s.DB.NextSeq()

		if res.RowCount() == 0 {
			// Records arrived but none matched. Not worth saying on every
			// batch; the status line already said what the filter is.
			streamErr = ctx.Err()
			return streamErr
		}
		if err := emit(res); err != nil {
			streamErr = err
			return err
		}
		// Checked again after emitting, not only before. Load only tests the
		// context between sources, so a stream cancelled while its last batch
		// was being written would otherwise finish reporting success.
		streamErr = ctx.Err()
		return streamErr
	}

	load, err := s.DB.Load(ctx, s.pending, opts)
	s.Load = load

	switch {
	case streamErr != nil:
		return streamErr
	case err != nil:
		return err
	case ctx.Err() != nil:
		return ctx.Err()
	}

	// A stream that ended without ever satisfying the filter still owes an
	// explanation, and the planning error is the honest one.
	if !havePlan && filter != "" {
		s.invalidateSchema()
		if _, err := s.Plan(ctx, filter); err != nil {
			return streamPlanError(err)
		}
	}
	return nil
}

// Drain reads the pending sources to the end, discarding nothing.
//
// It is what a command that cannot answer until the read has finished calls:
// grouping messages into templates, drawing a histogram, or running SQL over
// the lot. None of those can say anything true about records that have not
// arrived, so they wait for the pipe to close rather than reporting on an
// empty table — which is what they did before this existed, and it looked
// exactly like a directory with nothing in it.
func (s *Session) Drain(ctx context.Context) error {
	if !s.Streaming() {
		return nil
	}

	load, err := s.DB.Load(ctx, s.pending, s.load)
	s.Load = load
	s.pending = nil
	if err != nil {
		return err
	}

	// The schema was computed over an empty table, if at all.
	s.invalidateSchema()

	// Promotion is safe now: the appender is closed and no more records are
	// coming, so the table can be rewritten. A drained stream therefore gets
	// the same typed columns a file would, which is why this is worth doing
	// rather than just querying the bag.
	promoted, _, err := s.DB.InferAndPromote(ctx, schema.Options{})
	if err != nil {
		return err
	}
	s.Promoted = promoted
	return nil
}

// openStreaming builds a session over sources that arrive over time.
//
// The database is created but nothing is read: the caller drives the read
// through Stream, so that records can be shown while they are still arriving.
// It is always in memory, because a stream is uncacheable by definition — the
// same bytes will not be there to re-read.
func openStreaming(opts Options, sources []source.Source, walk source.WalkOptions) (*Session, error) {
	db, err := store.Open("")
	if err != nil {
		return nil, err
	}

	return &Session{
		DB:          db,
		Paths:       opts.Paths,
		Loc:         opts.Location,
		Walk:        walk,
		CacheReason: "a source is a stream and cannot be cached",
		load:        store.LoadOptions{Parser: opts.Parser, SourceZones: opts.SourceZones},
		walkOpts:    opts.Walk,

		relativeToNow: opts.RelativeToNow,
		pending:       sources,
	}, nil
}

// invalidateSchema drops the cached view of what the data contains.
//
// A stream's schema grows as records arrive, so a view computed over an empty
// table would say no fields exist and every filter would be a typo.
func (s *Session) invalidateSchema() {
	s.schema = nil
	s.haveCounts = false
}

// Streaming reports whether this session reads sources that arrive over time,
// and must therefore be driven through Stream rather than queried directly.
func (s *Session) Streaming() bool { return len(s.pending) > 0 }

// streamPlanError adds what a stream can add to a failure to resolve a filter:
// it was resolved against the records that had arrived, not against every
// record that ever will.
//
// Only unknown-field errors get the note. A syntax error means the same thing
// on a stream as on a file, and explaining streams at someone who typed a
// stray bracket would be noise.
func streamPlanError(err error) error {
	var unknown *query.UnknownFieldError
	if !errors.As(err, &unknown) {
		return err
	}
	return fmt.Errorf("%w\n(a stream's filter is resolved against the records that have "+
		"arrived so far, so a field appearing only later is not known yet)", err)
}
