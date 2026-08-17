package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/GrantPukka/loupe/internal/schema"
	"github.com/GrantPukka/loupe/internal/source"
)

// PollInterval is how often follow mode restats its files.
//
// Polling rather than filesystem notifications: no new dependency, identical
// behaviour on every platform, and it keeps working on NFS and bind mounts,
// where inotify silently misses events — a log tool that quietly stops showing
// new lines is worse than one that never offered to.
//
// 400ms reads as live to a human while costing one stat per file per tick.
const PollInterval = 400 * time.Millisecond

// stagingTable is where new records land before being merged into logs.
const stagingTable = "loupe_incoming"

// Follower appends what is written to a set of sources after the initial load.
//
// It holds no goroutines and owns no schedule. Poll does one pass and returns;
// the caller decides when to call it again and when to stop. That keeps the
// "no daemon" promise literal — following happens only while someone is
// watching, and stops the moment they stop asking.
type Follower struct {
	db      *DB
	resolve func() ([]source.Source, error)
	opts    LoadOptions
	states  map[string]fileState
	promos  []schema.Promotion
}

// Batch is what one poll made newly visible.
type Batch struct {
	// FromSeq is the sequence number of the first record this poll wrote.
	FromSeq int64

	// Records is how many records the caller should treat as new — the rows
	// this poll wrote, less any boundary record it merely re-read.
	Records int64

	// boundaries are records re-read to complete them, keyed by file to the
	// line number involved. They already reached the caller on an earlier poll,
	// so re-emitting them would print a duplicate line into a live stream.
	boundaries map[string]int64

	// Errors are per-source read failures. A file that fails to read must not
	// stop the others or end the session: during an incident the remaining
	// sources are still the best information available.
	Errors []error
}

// Predicate returns SQL selecting exactly the records this poll made newly
// visible, together with its arguments.
//
// It is not simply seq >= FromSeq. A record that was still being written when
// the last poll ran is re-read to complete it, which deletes and reinserts it
// under a new sequence number — so a range alone would print a line the user
// has already seen.
//
// The consequence, deliberately accepted: a record that gains continuation
// lines after it was first emitted is corrected in the store but not reprinted.
// Showing the same line twice in a live tail is worse than showing it once and
// letting the stored copy be the complete one.
func (b Batch) Predicate() (string, []any) {
	sql := "seq >= ?"
	args := []any{b.FromSeq}

	for file, line := range b.boundaries {
		sql += " AND NOT (file = ? AND line_no = ?)"
		args = append(args, file, line)
	}
	return sql, args
}

// NewFollower prepares to follow sources already loaded into db.
//
// resolve is called on every poll rather than the source list being captured
// once. Two reasons: a Source records the size and mtime it was walked with, so
// a captured one never appears to grow; and a service that starts logging
// during an incident creates a file that was not there at startup, which is
// exactly when you least want to be told to restart the tool.
func (s *DB) NewFollower(ctx context.Context, resolve func() ([]source.Source, error), opts LoadOptions) (*Follower, error) {
	states, err := readFileStates(ctx, s)
	if err != nil {
		return nil, err
	}

	promos, err := s.Promotions(ctx)
	if err != nil {
		return nil, err
	}

	return &Follower{db: s, resolve: resolve, opts: opts, states: states, promos: promos}, nil
}

// Poll reads whatever has been appended to the sources since the last call.
//
// Nothing new is not an error: it is the normal case, and returns an empty
// Batch.
func (f *Follower) Poll(ctx context.Context) (Batch, error) {
	current, err := f.resolve()
	if err != nil {
		return Batch{}, fmt.Errorf("re-read sources: %w", err)
	}

	decisions, err := plan(current, f.states)
	if err != nil {
		return Batch{}, err
	}

	var (
		toRead     []source.Source
		resumes    = map[string]Resume{}
		boundaries = map[string]int64{}
	)
	for _, d := range decisions {
		if d.Action == actionSkip {
			continue
		}
		toRead = append(toRead, d.Source)

		if d.Action == actionAppend {
			resumes[d.Source.Name()] = d.Resume
			boundaries[d.Source.Name()] = d.FromLine
		}
		// Both an append and a reread discard first: the append drops only the
		// boundary record it is about to read again, a reread drops the lot.
		if err := f.db.discardFrom(ctx, d.Source.Name(), d.FromLine); err != nil {
			return Batch{}, err
		}
	}

	if len(toRead) == 0 {
		return Batch{}, nil
	}

	from := f.db.seq
	opts := f.opts
	opts.Resume = resumes
	opts.Table = stagingTable

	_, load, err := f.ingestStaged(ctx, toRead, opts)
	if err != nil {
		return Batch{}, err
	}

	for _, r := range load.Results {
		src := sourceNamed(toRead, r.Source.File)
		if src == nil {
			continue
		}
		f.states[r.Source.File] = stateFor(src, r, f.states[r.Source.File].Before)
	}

	batch := Batch{FromSeq: from, boundaries: boundaries, Errors: load.Errors}

	// Counted through the same predicate the caller will use, so the number
	// reported and the records emitted can never disagree.
	where, args := batch.Predicate()
	if err := f.db.QueryRow(ctx,
		`SELECT count(*) FROM logs WHERE `+where, args...).Scan(&batch.Records); err != nil {
		return Batch{}, fmt.Errorf("count new records: %w", err)
	}
	return batch, nil
}

// ingestStaged reads into a staging table, then merges into logs.
//
// The appender writes the base column set and logs has been widened by schema
// inference, so records cannot be appended to it directly. Staging and then
// inserting with the promoted columns computed keeps the cost proportional to
// what arrived, where rebuilding the whole table would make every poll cost the
// size of the dataset.
func (f *Follower) ingestStaged(ctx context.Context, sources []source.Source, opts LoadOptions) (int64, Load, error) {
	if err := f.db.createStaging(ctx); err != nil {
		return 0, Load{}, err
	}
	defer func() { _ = f.db.Exec(ctx, `DROP TABLE IF EXISTS `+stagingTable) }()

	load, err := f.db.Load(ctx, sources, opts)
	if err != nil {
		return 0, load, err
	}

	added, err := f.db.mergeStaging(ctx, f.promos)
	if err != nil {
		return 0, load, err
	}
	return added, load, nil
}

// createStaging makes an empty table with the base column shape.
func (s *DB) createStaging(ctx context.Context) error {
	cols := make([]string, len(Columns))
	for i, c := range Columns {
		cols[i] = quoteIdent(c)
	}

	if err := s.Exec(ctx, `DROP TABLE IF EXISTS `+stagingTable); err != nil {
		return fmt.Errorf("clear staging table: %w", err)
	}
	// WHERE false copies the column types from logs without any rows, so the
	// staging shape cannot drift from what the appender writes.
	err := s.Exec(ctx, `CREATE TABLE `+stagingTable+` AS SELECT `+
		strings.Join(cols, ", ")+` FROM logs WHERE false`)
	if err != nil {
		return fmt.Errorf("create staging table: %w", err)
	}
	return nil
}

// mergeStaging inserts the staged records into logs, computing the promoted
// columns from the fields bag as schema inference would have.
//
// Columns are named explicitly rather than relying on position. A promoted
// column list that drifted out of order would put values in the wrong columns
// silently, which is worse than failing.
func (s *DB) mergeStaging(ctx context.Context, promos []schema.Promotion) (int64, error) {
	names := make([]string, 0, len(Columns)+len(promos))
	values := make([]string, 0, len(Columns)+len(promos))

	for _, c := range Columns {
		names = append(names, quoteIdent(c))
		values = append(values, quoteIdent(c))
	}
	for _, p := range promos {
		names = append(names, quoteIdent(p.Column))
		values = append(values, fmt.Sprintf("TRY_CAST(%s AS %s)", jsonExtract(p.Field), p.Kind.SQLType()))
	}

	var n int64
	row := s.QueryRow(ctx, `SELECT count(*) FROM `+stagingTable)
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count staged records: %w", err)
	}
	if n == 0 {
		return 0, nil
	}

	err := s.Exec(ctx, `INSERT INTO logs (`+strings.Join(names, ", ")+`) SELECT `+
		strings.Join(values, ", ")+` FROM `+stagingTable)
	if err != nil {
		return 0, fmt.Errorf("merge staged records: %w", err)
	}
	return n, nil
}
