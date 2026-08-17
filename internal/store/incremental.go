package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/GrantPukka/loupe/internal/parse"
	"github.com/GrantPukka/loupe/internal/source"
)

// cacheFilesTable records where reading each file stopped, so a later run can
// append what was written since instead of re-reading everything.
//
// This is what makes the cache useful on a live directory. Keyed by path
// because that is what the walker produces and what the logs table stores.
const cacheFilesTable = `
CREATE TABLE IF NOT EXISTS loupe_cache_files (
    path         VARCHAR NOT NULL,
    head         VARCHAR NOT NULL,  -- hash of the first bytes; detects a rewrite
    size         BIGINT  NOT NULL,
    resume_at    BIGINT  NOT NULL,  -- byte offset of the last record read
    resume_line  BIGINT  NOT NULL,  -- that record's line number
    before       VARCHAR NOT NULL   -- JSON parse.Stats, excluding that record
)`

// fileState is one row of loupe_cache_files.
type fileState struct {
	Path       string
	Head       string
	Size       int64
	ResumeAt   int64
	ResumeLine int64

	// Before is the file's totals excluding its last record, which a resumed
	// read re-reads and re-counts. Adding it to the resumed read's stats gives
	// the file's true totals.
	Before parse.Stats
}

// action is what a source needs on re-open.
type action int

const (
	// actionSkip means the file is byte-for-byte what was already ingested.
	actionSkip action = iota
	// actionAppend means the file grew; read from the stored offset.
	actionAppend
	// actionReread means the file was rewritten, truncated, or is new; drop
	// whatever was ingested from it and read it whole.
	actionReread
)

// decision pairs a source with what to do about it.
type decision struct {
	Source source.Source
	Action action
	Resume Resume
	// FromLine is the line number at or above which existing rows for this file
	// must be discarded before appending. Zero means discard everything.
	FromLine int64
}

// plan decides what to do with each source given what was previously ingested.
//
// The head hash is the load-bearing check. Size alone cannot tell appending
// from rewriting: a file truncated and refilled to the same length looks
// identical by size and would silently serve records that no longer exist.
func plan(sources []source.Source, states map[string]fileState) ([]decision, error) {
	out := make([]decision, 0, len(sources))

	for _, src := range sources {
		prev, known := states[src.Name()]
		if !known {
			out = append(out, decision{Source: src, Action: actionReread})
			continue
		}

		tail, ok := src.(source.Tailable)
		if !ok {
			out = append(out, decision{Source: src, Action: actionReread})
			continue
		}

		// Hashed over the length recorded last time, so the comparison covers
		// the same bytes. Hashing a fixed window would make every append to a
		// file smaller than that window look like a rewrite.
		head, err := tail.Head(headLen(prev.Size))
		if err != nil {
			// The file cannot be read — deleted, renamed, or permissions
			// changed since the walk. Keep what was already ingested from it.
			//
			// Re-reading would fail anyway, and discarding would delete records
			// we hold and can still answer questions about, on no evidence that
			// they are wrong. Losing data because a file became unreadable is
			// exactly the silent loss this project refuses.
			out = append(out, decision{Source: src, Action: actionSkip})
			continue
		}

		switch {
		case head != prev.Head || src.Size() < prev.Size:
			// Rewritten, rotated, or truncated. Nothing previously read from
			// this path can be trusted.
			out = append(out, decision{Source: src, Action: actionReread})

		case src.Size() == prev.Size:
			out = append(out, decision{Source: src, Action: actionSkip})

		default:
			// Grew. Re-read the last record so a record that was mid-write when
			// we stopped is completed rather than orphaned.
			out = append(out, decision{
				Source:   src,
				Action:   actionAppend,
				Resume:   Resume{Offset: prev.ResumeAt, LastLine: prev.ResumeLine - 1},
				FromLine: prev.ResumeLine,
			})
		}
	}

	return out, nil
}

// readFileStates loads the per-file offsets recorded by the previous run.
func readFileStates(ctx context.Context, db *DB) (map[string]fileState, error) {
	if err := db.Exec(ctx, cacheFilesTable); err != nil {
		return nil, err
	}

	rows, err := db.Query(ctx,
		`SELECT path, head, size, resume_at, resume_line, before FROM loupe_cache_files`)
	if err != nil {
		return nil, fmt.Errorf("read cached file offsets: %w", err)
	}
	defer rows.Close()

	states := map[string]fileState{}
	for rows.Next() {
		var (
			s   fileState
			raw string
		)
		if err := rows.Scan(&s.Path, &s.Head, &s.Size, &s.ResumeAt, &s.ResumeLine, &raw); err != nil {
			return nil, fmt.Errorf("scan cached file offset: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &s.Before); err != nil {
			return nil, fmt.Errorf("decode stats for %s: %w", s.Path, err)
		}
		states[s.Path] = s
	}
	return states, rows.Err()
}

// writeFileStates replaces the recorded offsets with the current ones.
func writeFileStates(ctx context.Context, db *DB, states map[string]fileState) error {
	if err := db.Exec(ctx, cacheFilesTable); err != nil {
		return err
	}
	if err := db.Exec(ctx, `DELETE FROM loupe_cache_files`); err != nil {
		return fmt.Errorf("clear cached file offsets: %w", err)
	}

	for _, s := range states {
		raw, err := json.Marshal(s.Before)
		if err != nil {
			return fmt.Errorf("encode stats for %s: %w", s.Path, err)
		}
		if err := db.Exec(ctx,
			`INSERT INTO loupe_cache_files (path, head, size, resume_at, resume_line, before)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			s.Path, s.Head, s.Size, s.ResumeAt, s.ResumeLine, string(raw)); err != nil {
			return fmt.Errorf("record offset for %s: %w", s.Path, err)
		}
	}
	return nil
}

// stateFor builds the row to store for a source after reading it.
//
// before carries forward what earlier reads of this file already counted, so
// the stored totals describe the whole file rather than the last read of it.
func stateFor(src source.Source, result IngestResult, before parse.Stats) fileState {
	cumulative := before
	cumulative.Add(result.Before)

	s := fileState{
		Path:       src.Name(),
		Size:       src.Size(),
		ResumeAt:   result.ResumeAt,
		ResumeLine: result.ResumeLine,
		Before:     cumulative,
	}
	if tail, ok := src.(source.Tailable); ok {
		if head, err := tail.Head(headLen(s.Size)); err == nil {
			s.Head = head
		}
	}
	return s
}

// headLen is how much of a file to hash: the whole thing when it is small,
// capped otherwise. Recorded implicitly as the size stored beside the hash.
func headLen(size int64) int64 {
	if size > source.HeadBytes {
		return source.HeadBytes
	}
	return size
}

// discardFrom removes previously ingested rows for a file.
//
// fromLine of zero clears the file entirely, which is what a rewrite needs. A
// higher value clears only the trailing record being re-read, which is what
// makes an append idempotent.
func (s *DB) discardFrom(ctx context.Context, file string, fromLine int64) error {
	if fromLine <= 0 {
		if err := s.Exec(ctx, `DELETE FROM logs WHERE file = ?`, file); err != nil {
			return fmt.Errorf("discard records for %s: %w", file, err)
		}
		return nil
	}

	if err := s.Exec(ctx,
		`DELETE FROM logs WHERE file = ? AND line_no >= ?`, file, fromLine); err != nil {
		return fmt.Errorf("discard tail of %s: %w", file, err)
	}
	return nil
}

// discardMissing removes rows for files that are no longer on disk.
//
// The cache mirrors the current source set. Serving records from a file that
// has since been deleted would be a claim about the filesystem that is no
// longer true, and the user has no way to notice it.
func (s *DB) discardMissing(ctx context.Context, present map[string]bool, states map[string]fileState) error {
	for path := range states {
		if present[path] {
			continue
		}
		if err := s.discardFrom(ctx, path, 0); err != nil {
			return err
		}
		delete(states, path)
	}
	return nil
}

// refresh brings a cached database up to date with what the files now hold.
//
// Unchanged files are left alone, grown files are appended from their stored
// offset, and rewritten or vanished files have their records discarded first.
// The returned Load describes the whole dataset, not just what this call read:
// a status line that reported only the delta would understate the data on
// screen, and every count loupe prints is a claim about all of it.
func refresh(ctx context.Context, db *DB, prev Load, sources []source.Source, opts LoadOptions) (Load, int, error) {
	states, err := readFileStates(ctx, db)
	if err != nil {
		return prev, 0, err
	}

	present := make(map[string]bool, len(sources))
	for _, src := range sources {
		present[src.Name()] = true
	}
	if err := db.discardMissing(ctx, present, states); err != nil {
		return prev, 0, err
	}

	decisions, err := plan(sources, states)
	if err != nil {
		return prev, 0, err
	}

	// Results from the previous run, so an untouched file keeps its numbers.
	byFile := make(map[string]IngestResult, len(prev.Results))
	for _, r := range prev.Results {
		byFile[r.Source.File] = r
	}

	var (
		toRead  []source.Source
		resumes = map[string]Resume{}
		changed int
	)

	for _, d := range decisions {
		if d.Action == actionSkip {
			continue
		}
		changed++
		toRead = append(toRead, d.Source)

		if d.Action == actionAppend {
			resumes[d.Source.Name()] = d.Resume
			if err := db.discardFrom(ctx, d.Source.Name(), d.FromLine); err != nil {
				return prev, 0, err
			}
			continue
		}

		// A reread replaces everything previously ingested from this path.
		if err := db.discardFrom(ctx, d.Source.Name(), 0); err != nil {
			return prev, 0, err
		}
		delete(states, d.Source.Name())
		delete(byFile, d.Source.Name())
	}

	if changed == 0 {
		return prev, 0, nil
	}

	// The Appender writes the base column set, so the table has to lose its
	// promoted columns before anything can be added. Inference runs again after
	// the load and rebuilds them, which is also what picks up a field that only
	// appears in the newly written records.
	if err := db.demote(ctx); err != nil {
		return prev, 0, err
	}

	opts.Resume = resumes
	fresh, err := db.Load(ctx, toRead, opts)
	if err != nil {
		return prev, 0, err
	}

	for _, r := range fresh.Results {
		carried := states[r.Source.File].Before

		merged := r
		merged.Stats = carried
		merged.Stats.Add(r.Stats)
		byFile[r.Source.File] = merged

		states[r.Source.File] = stateFor(sourceNamed(toRead, r.Source.File), r, carried)
	}

	if err := writeFileStates(ctx, db, states); err != nil {
		return prev, 0, err
	}

	return rebuildLoad(sources, byFile, fresh.Errors, prev.Took+fresh.Took), changed, nil
}

// sourceNamed finds the source a result came from.
func sourceNamed(sources []source.Source, name string) source.Source {
	for _, s := range sources {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// rebuildLoad reassembles a Load covering every current source, in walk order.
//
// Ordering follows the sources rather than the map, because the status line and
// the source list are read by humans and must not shuffle between runs.
func rebuildLoad(sources []source.Source, byFile map[string]IngestResult, errs []error, took time.Duration) Load {
	out := Load{Errors: errs, Took: took}

	for _, src := range sources {
		r, ok := byFile[src.Name()]
		if !ok {
			continue
		}
		out.Results = append(out.Results, r)
		out.Stats.Add(r.Stats)
	}
	return out
}

// plural renders a count with its noun, so status lines read as English.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// statesFrom builds the file-state rows for a complete, from-scratch read.
func statesFrom(sources []source.Source, load Load) map[string]fileState {
	byFile := make(map[string]IngestResult, len(load.Results))
	for _, r := range load.Results {
		byFile[r.Source.File] = r
	}

	out := make(map[string]fileState, len(sources))
	for _, s := range sources {
		r, ok := byFile[s.Name()]
		if !ok {
			// A source that failed to read has no trustworthy offset. Leaving it
			// out means the next run reads it from the start and reports the
			// failure again, rather than silently skipping it forever.
			continue
		}
		out[s.Name()] = stateFor(s, r, parse.Stats{})
	}
	return out
}

// demote returns the logs table to its base columns, discarding the typed
// columns that schema inference added.
//
// The Appender writes the base column set, so it cannot append to a table that
// promotion has widened — and a cached database always has been widened. No
// data is lost: every promoted column is derived from the fields bag, which is
// still there, so re-running inference after the append reconstructs them and
// picks up any field the new records introduced.
func (s *DB) demote(ctx context.Context) error {
	wide, err := s.isPromoted(ctx)
	if err != nil {
		return err
	}
	if !wide {
		return nil
	}

	cols := make([]string, len(Columns))
	for i, c := range Columns {
		cols[i] = quoteIdent(c)
	}

	for _, stmt := range []string{
		`DROP TABLE IF EXISTS logs_base`,
		`CREATE TABLE logs_base AS SELECT ` + strings.Join(cols, ", ") + ` FROM logs`,
		`DROP TABLE logs`,
		`ALTER TABLE logs_base RENAME TO logs`,
	} {
		if err := s.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("demote promoted columns: %w", err)
		}
	}
	return nil
}

// isPromoted reports whether the logs table carries more than the base columns.
func (s *DB) isPromoted(ctx context.Context) (bool, error) {
	var n int
	err := s.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns WHERE table_name = 'logs'`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("inspect logs columns: %w", err)
	}
	return n > len(Columns), nil
}
