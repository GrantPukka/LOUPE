package store

import (
	"context"
	"fmt"
)

// fieldNamesTable records the field names present in the bag, so that a session
// does not have to rediscover them by reading every record.
//
// Finding them means parsing the JSON bag of every row — on a 200MB corpus that
// is 450ms, which was 88% of the cost of answering an already-cached question.
// The names are a property of the ingest, not of the question being asked, so
// they belong beside the promotion decisions rather than in the hot path of
// every command.
//
// rows is the record count they were derived from. A cache that has had records
// appended to it since has a list that may be missing a name, and a missing
// name is not a slow answer but a wrong one: the filter language answers an
// unknown field with an error listing what exists, so a stale list would refuse
// a field that is really there. Comparing the count is cheap and makes the list
// self-invalidating.
const fieldNamesTable = `
CREATE TABLE IF NOT EXISTS loupe_field_names (
    rows BIGINT  NOT NULL,
    name VARCHAR NOT NULL
)`

// StoredFieldNames returns the cached field names, and whether they can be
// trusted for the data currently in the table.
func (s *DB) StoredFieldNames(ctx context.Context) ([]string, bool, error) {
	var exists bool
	row := s.QueryRow(ctx,
		`SELECT count(*) > 0 FROM information_schema.tables WHERE table_name = 'loupe_field_names'`)
	if err := row.Scan(&exists); err != nil || !exists {
		return nil, false, nil
	}

	count, err := s.Count(ctx)
	if err != nil {
		return nil, false, err
	}

	rows, err := s.Query(ctx, `SELECT rows, name FROM loupe_field_names ORDER BY name`)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var (
		out    []string
		wrote  int64
		seeded bool
	)
	for rows.Next() {
		var name string
		if err := rows.Scan(&wrote, &name); err != nil {
			return nil, false, fmt.Errorf("scan field name: %w", err)
		}
		seeded = true
		// The empty name is the "no bag fields at all" sentinel, not a field.
		if name != "" {
			out = append(out, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	// Records were added after the list was written, so it may be short of a
	// name. Recompute rather than answer from it.
	if !seeded || wrote != count {
		return nil, false, nil
	}
	return out, true, nil
}

// StoreFieldNames records the field names against the current record count.
func (s *DB) StoreFieldNames(ctx context.Context, names []string) error {
	count, err := s.Count(ctx)
	if err != nil {
		return err
	}
	if err := s.Exec(ctx, fieldNamesTable); err != nil {
		return err
	}
	if err := s.Exec(ctx, `DELETE FROM loupe_field_names`); err != nil {
		return err
	}

	// A corpus with no bag fields at all still writes a row, so that "nothing
	// to record" is distinguishable from "never recorded" and does not send
	// every later command back to the full scan.
	if len(names) == 0 {
		return s.Exec(ctx, `INSERT INTO loupe_field_names VALUES (?, '')`, count)
	}
	for _, name := range names {
		if err := s.Exec(ctx, `INSERT INTO loupe_field_names VALUES (?, ?)`, count, name); err != nil {
			return err
		}
	}
	return nil
}
