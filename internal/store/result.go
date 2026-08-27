package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Result is a materialised query result.
//
// Every consumer — the table renderer, the JSON renderer, the HTTP API, and the
// handoff writer — reads this same shape, which is what guarantees the exported
// records cannot differ from the ones on screen.
type Result struct {
	Columns []string
	Types   []string
	Rows    [][]any

	// Total is the number of rows the query matched, which is larger than
	// len(Rows) when a limit truncated the output.
	Total int64

	// Truncated says the displayed rows are not all of them. Output that does
	// not admit it was truncated is worse than no output.
	Truncated bool

	Took time.Duration
}

// RowCount is the number of rows actually returned.
func (r Result) RowCount() int { return len(r.Rows) }

// QueryResult runs a query and materialises it, applying a row limit.
//
// A limit of zero means no limit. When a limit truncates the output, Total is
// filled from a separate count so the caller can state the real figure rather
// than implying the displayed rows are everything.
func (s *DB) QueryResult(ctx context.Context, limit int, query string, args ...any) (Result, error) {
	start := time.Now()

	rows, err := s.Query(ctx, query, args...)
	if err != nil {
		return Result{}, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return Result{}, fmt.Errorf("columns: %w", err)
	}

	types, err := rows.ColumnTypes()
	if err != nil {
		return Result{}, fmt.Errorf("column types: %w", err)
	}

	res := Result{Columns: cols, Types: make([]string, len(types))}
	for i, t := range types {
		res.Types[i] = t.DatabaseTypeName()
	}

	// Read one row past the limit, so truncation is detected without a second
	// query in the common case.
	for rows.Next() {
		if limit > 0 && len(res.Rows) == limit {
			res.Truncated = true
			break
		}

		scan := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range scan {
			ptrs[i] = &scan[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return Result{}, fmt.Errorf("scan: %w", err)
		}
		res.Rows = append(res.Rows, scan)
	}
	if err := rows.Err(); err != nil {
		return Result{}, fmt.Errorf("iterate rows: %w", err)
	}

	res.Total = int64(len(res.Rows))
	if res.Truncated {
		total, err := s.countOf(ctx, query, args...)
		if err != nil {
			return Result{}, err
		}
		res.Total = total
	}

	res.Took = time.Since(start)
	return res, nil
}

// countOf counts the rows a query would return, by wrapping it. The wrapped
// query is our own SQL around an already-parameterised statement, so no user
// input is concatenated here.
func (s *DB) countOf(ctx context.Context, query string, args ...any) (int64, error) {
	var n int64
	row := s.QueryRow(ctx, `SELECT count(*) FROM (`+query+`)`, args...)
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count query rows: %w", err)
	}
	return n, nil
}

// Fields returns the distinct keys present in the fields column.
//
// The DSL needs this to tell a user which field names exist when they typo one.
// An unknown field name must produce a suggestion, never an empty result.
func (s *DB) Fields(ctx context.Context) ([]string, error) {
	rows, err := s.Query(ctx, `
		SELECT DISTINCT unnest(json_keys(fields)) AS key
		FROM logs
		WHERE fields IS NOT NULL
		ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan field key: %w", err)
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

// SourceInfo is one row of `loupe sources`.
type SourceInfo struct {
	Name        string
	File        string
	Format      string
	Records     int64
	Unparsed    int64
	NoTimestamp int64
	ZoneAssumed int64
	Oldest      sql.NullTime
	Newest      sql.NullTime
}

// TimezoneStatus renders the known-or-assumed verdict for this source.
//
// A receiver who does not know an assumption was made cannot check it, so this
// distinction is stated for every file rather than only the interesting ones.
func (s SourceInfo) TimezoneStatus() string {
	timestamped := s.Records - s.NoTimestamp

	switch {
	case timestamped == 0:
		// Claiming a known timezone for a file with no timestamps at all would
		// be a reassuring lie.
		return "n/a — no timestamps"
	case s.ZoneAssumed == 0:
		return "known (carried in the format)"
	case s.ZoneAssumed == timestamped:
		return "assumed — no offset in format"
	default:
		return fmt.Sprintf("mixed — %d of %d assumed", s.ZoneAssumed, timestamped)
	}
}

// Sources lists every ingested file with its format and timezone provenance.
func (s *DB) Sources(ctx context.Context) ([]SourceInfo, error) {
	rows, err := s.Query(ctx, `
		SELECT source, file, format,
		       count(*),
		       count(*) FILTER (WHERE NOT parsed),
		       count(*) FILTER (WHERE ts IS NULL),
		       count(*) FILTER (WHERE ts IS NOT NULL AND NOT ts_zoned),
		       min(ts), max(ts)
		FROM logs
		GROUP BY source, file, format
		-- Ordered all the way down to a unique key. A file read line by line
		-- yields one row per format it turned out to contain, and those rows
		-- share a source and a file — so ordering by those two alone left the
		-- rest to the engine, and two runs over an unchanged cache listed the
		-- same formats in different orders. Detection goes out of its way to be
		-- deterministic; the report of it should match.
		ORDER BY source, file, count(*) DESC, format`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SourceInfo
	for rows.Next() {
		var si SourceInfo
		if err := rows.Scan(&si.Name, &si.File, &si.Format, &si.Records,
			&si.Unparsed, &si.NoTimestamp, &si.ZoneAssumed, &si.Oldest, &si.Newest); err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// DecodeFields parses a stored fields bag.
//
// The column holds JSON text rather than a JSON-typed value, for the reason
// recorded on the schema in store.go. Callers that need the values back as Go
// types go through here rather than each writing their own unmarshal.
func DecodeFields(raw string) (map[string]any, error) {
	if raw == "" {
		return nil, nil
	}

	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("decode fields: %w", err)
	}
	return out, nil
}
