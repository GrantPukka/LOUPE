package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/VIGIL-OPS/loupe/internal/query"
)

// compile resolves a parsed query against the loaded data's schema.
//
// The schema comes from the store rather than being assumed, which is what
// makes an unknown field name an error naming the fields that actually exist
// instead of a guess.
func (s *session) compile(ctx context.Context, q query.Query) (query.SQL, error) {
	schema, err := s.querySchema(ctx)
	if err != nil {
		return query.SQL{}, err
	}
	return query.Compile(q, schema)
}

func (s *session) querySchema(ctx context.Context) (query.Schema, error) {
	if s.schema != nil {
		return *s.schema, nil
	}

	fields, err := s.db.Fields(ctx)
	if err != nil {
		return query.Schema{}, err
	}

	infos, err := s.db.Sources(ctx)
	if err != nil {
		return query.Schema{}, err
	}

	schema := query.Schema{Fields: fields}
	seen := map[string]bool{}
	for _, info := range infos {
		if !seen[info.Name] {
			seen[info.Name] = true
			schema.Sources = append(schema.Sources, info.Name)
		}
	}
	sort.Strings(schema.Sources)

	s.schema = &schema
	return schema, nil
}

// explainEmpty says why a filter matched nothing.
//
// An empty table with no explanation is the most misleading output this tool
// can produce: the user cannot tell whether their filter was wrong or their
// logs genuinely contain nothing. Narrowing the query one term at a time finds
// the term responsible and names it.
func (s *session) explainEmpty(ctx context.Context, q query.Query) error {
	if len(q.Terms) == 1 {
		fmt.Fprintf(os.Stderr, "\nNo records matched %s.\n", q.Terms[0].String())
		return nil
	}

	schema, err := s.querySchema(ctx)
	if err != nil {
		return nil // The result is already correct; this is only commentary.
	}

	// Find the terms that match nothing on their own. Those are the ones worth
	// naming; a term that matches plenty alone is only guilty in combination.
	var barren []string
	for _, term := range q.Terms {
		n, err := s.countMatching(ctx, query.Query{Terms: []query.Term{term}}, schema)
		if err != nil {
			continue
		}
		if n == 0 {
			barren = append(barren, term.String())
		}
	}

	fmt.Fprintln(os.Stderr)
	switch len(barren) {
	case 0:
		fmt.Fprintln(os.Stderr, "No records matched. Each term matches something on its own, "+
			"so it is the combination that excludes everything.")
	default:
		fmt.Fprintf(os.Stderr, "No records matched. These terms match nothing on their own: %s\n",
			strings.Join(barren, ", "))
	}
	return nil
}

func (s *session) countMatching(ctx context.Context, q query.Query, schema query.Schema) (int64, error) {
	sql, err := query.Compile(q, schema)
	if err != nil {
		return 0, err
	}

	var n int64
	row := s.db.QueryRow(ctx, `SELECT count(*) FROM logs WHERE `+sql.Where, sql.Args...)
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
