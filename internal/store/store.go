package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/marcboeker/go-duckdb"
)

// Schema is the logs table.
//
// ts is nullable rather than zero-valued: a record with no parseable timestamp
// is still a record, and NULL is what lets ts:none select exactly those while
// keeping them out of range comparisons. A zero timestamp would sort them into
// the year 1 and quietly pollute every window.
//
// raw is always populated, for every record, parsed or not. Handoffs include it
// because the receiver may not trust our parser.
const Schema = `
CREATE TABLE IF NOT EXISTS logs (
    seq       BIGINT NOT NULL,   -- global ingest order, for stable sorting
    ts        TIMESTAMP,         -- NULL when the line carried no timestamp
    ts_zoned  BOOLEAN,           -- false when ts came from an assumed timezone
    level     VARCHAR,
    message   VARCHAR,
    source    VARCHAR,           -- logical source, e.g. checkout-api
    file      VARCHAR,           -- path it came from
    format    VARCHAR,           -- parser that read it
    line_no   BIGINT,
    parsed    BOOLEAN,           -- false when no parser understood the line
    raw       VARCHAR NOT NULL,  -- the original text, always
    fields    VARCHAR,           -- unpromoted fields, JSON text (see below)

    -- The message with its variable parts masked, and a stable name for that
    -- shape. Computed at ingest rather than per query: pattern:<id> has to
    -- compile to an ordinary predicate, and deriving templates in SQL would
    -- mean a second implementation of the masking rules that could disagree
    -- with the Go one. See internal/pattern.
    pattern    VARCHAR,
    pattern_id VARCHAR
)`

// fields is VARCHAR rather than JSON on purpose.
//
// Appending a Go string into a JSON-typed column stores it as a JSON *string
// value*, not an object: json_type reports VARCHAR and every extraction
// silently returns NULL. That failure is invisible — queries succeed and
// return nothing — which is precisely the confident-wrong-answer mode this
// project exists to avoid.
//
// DuckDB's JSON functions and the -> and ->> operators accept VARCHAR and cast
// implicitly, so storing the text directly costs nothing and behaves correctly.
// There is a test pinning this.

// Columns is the logs table's column order, which the Appender depends on.
var Columns = []string{
	"seq", "ts", "ts_zoned", "level", "message",
	"source", "file", "format", "line_no", "parsed", "raw", "fields",
	"pattern", "pattern_id",
}

// DB is a DuckDB instance holding ingested logs.
type DB struct {
	db        *sql.DB
	connector *duckdb.Connector
	path      string
	seq       int64
}

// Open creates a store. An empty path gives an in-memory database, which is
// what a --no-cache run uses.
func Open(path string) (*DB, error) {
	connector, err := duckdb.NewConnector(path, nil)
	if err != nil {
		return nil, fmt.Errorf("open duckdb %q: %w", path, err)
	}

	db := sql.OpenDB(connector)
	if err := db.Ping(); err != nil {
		db.Close()
		connector.Close()
		return nil, fmt.Errorf("ping duckdb %q: %w", path, err)
	}

	s := &DB{db: db, connector: connector, path: path}
	if _, err := db.Exec(Schema); err != nil {
		s.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	// Resume the sequence when reopening a cached database, so appended
	// records do not collide with existing ones.
	if err := db.QueryRow(`SELECT coalesce(max(seq), -1) + 1 FROM logs`).Scan(&s.seq); err != nil {
		s.Close()
		return nil, fmt.Errorf("read sequence: %w", err)
	}

	return s, nil
}

// Close releases the database.
func (s *DB) Close() error {
	var err error
	if s.db != nil {
		err = s.db.Close()
	}
	if s.connector != nil {
		if cerr := s.connector.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

// NextSeq is the sequence number the next ingested record will be given.
//
// Everything below it has been added, so a streaming caller uses it as the
// boundary between what it has already shown and what has yet to arrive.
func (s *DB) NextSeq() int64 { return s.seq }

// SQL exposes the underlying handle for the raw `loupe sql` path.
func (s *DB) SQL() *sql.DB { return s.db }

// Query runs parameterised SQL.
//
// Args are always passed as parameters, never interpolated. The filter DSL
// compiles to placeholders for this reason, and any code path here that builds
// SQL from user input by concatenation is a bug.
func (s *DB) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	return rows, nil
}

// QueryRow runs parameterised SQL returning a single row.
func (s *DB) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, query, args...)
}

// Exec runs a statement.
func (s *DB) Exec(ctx context.Context, query string, args ...any) error {
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return nil
}

// Count returns the number of ingested records.
func (s *DB) Count(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM logs`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count: %w", err)
	}
	return n, nil
}

// TimeRange returns the oldest and newest timestamps present, and how many
// records have none.
//
// The DSL needs all three: bare times resolve against the data's date range,
// last: anchors to the newest record rather than wall clock, and the excluded
// count is what a time filter has to report.
func (s *DB) TimeRange(ctx context.Context) (oldest, newest time.Time, noTimestamp int64, err error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT min(ts), max(ts), count(*) FILTER (WHERE ts IS NULL)
		FROM logs`)

	var lo, hi sql.NullTime
	if err := row.Scan(&lo, &hi, &noTimestamp); err != nil {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("time range: %w", err)
	}
	if lo.Valid {
		oldest = lo.Time
	}
	if hi.Valid {
		newest = hi.Time
	}
	return oldest, newest, noTimestamp, nil
}
