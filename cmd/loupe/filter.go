package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/VIGIL-OPS/loupe/internal/query"
)

// compile resolves a parsed query's time terms and field names against the
// loaded data, then compiles it to parameterised SQL.
//
// Both halves need the data: the schema so that an unknown field names the
// fields that actually exist, and the date range so that a bare 14:00 lands on
// a day the logs cover rather than on today.
func (s *session) compile(ctx context.Context, q query.Query) (query.SQL, error) {
	tc, err := s.timeContext(ctx)
	if err != nil {
		return query.SQL{}, err
	}

	resolved, resolution, err := query.ResolveTime(q, tc)
	if err != nil {
		return query.SQL{}, err
	}
	s.resolution = &resolution

	schema, err := s.querySchema(ctx)
	if err != nil {
		return query.SQL{}, err
	}
	return query.Compile(resolved, schema)
}

// timeContext gathers what time resolution needs from the loaded data.
func (s *session) timeContext(ctx context.Context) (query.TimeContext, error) {
	oldest, newest, noTimestamp, err := s.db.TimeRange(ctx)
	if err != nil {
		return query.TimeContext{}, err
	}
	s.noTimestamp = noTimestamp

	return query.TimeContext{
		Loc:           s.loc,
		Oldest:        oldest,
		Newest:        newest,
		Now:           time.Now(),
		RelativeToNow: s.relativeToNow,
	}, nil
}

// timeBanner prints the resolved window in both timezones, and every note the
// resolver produced.
//
// docs/FILTER-DSL.md section 2.3 calls this the feature rather than a nicety.
// Somebody working an incident at four in the morning should never have to do
// offset arithmetic, and the UTC line is what they paste into the ticket.
func (s *session) timeBanner(w io.Writer) {
	if s.resolution == nil || !s.resolution.HasTimeFilter() {
		s.printNotes(w)
		return
	}

	fmt.Fprintf(w, "Window: %s\n", s.resolution.Interval.Describe(s.loc))
	for _, ex := range s.resolution.Exclude {
		fmt.Fprintf(w, "Excluding: %s\n", ex.Describe(s.loc))
	}

	s.printNotes(w)

	// A time filter necessarily excludes records with no timestamp. Silently
	// dropping them is a bug, so the count is always stated and the term that
	// finds them is offered.
	if s.noTimestamp > 0 {
		fmt.Fprintf(w, "%d record(s) excluded for having no timestamp — use ts:none to inspect them\n",
			s.noTimestamp)
	}
}

func (s *session) printNotes(w io.Writer) {
	if s.resolution == nil {
		return
	}
	for _, note := range s.resolution.Notes {
		fmt.Fprintf(w, "Note: %s\n", note.Text)
	}
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

	schema := query.Schema{Fields: fields, Promoted: map[string]string{}}
	for _, p := range s.promoted {
		schema.Promoted[p.Field] = p.Column
	}

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
	schema, err := s.querySchema(ctx)
	if err != nil {
		return nil // The result is already correct; this is only commentary.
	}

	tc, err := s.timeContext(ctx)
	if err != nil {
		return nil
	}

	fmt.Fprintln(os.Stderr)

	// A window that misses the data entirely is the most common cause, and the
	// most fixable, so check it first and say what the data actually covers.
	if s.resolution != nil && s.resolution.HasTimeFilter() && !tc.Oldest.IsZero() {
		data := query.Interval{Start: tc.Oldest, End: tc.Newest.Add(time.Nanosecond)}
		if !overlaps(s.resolution.Interval, data) {
			fmt.Fprintf(os.Stderr, "No records in that window. The data covers %s.\n",
				data.Describe(s.loc))
			return nil
		}
	}

	// Otherwise narrow down which term is responsible. A term that matches
	// nothing alone is the culprit; one that matches plenty is only guilty in
	// combination.
	var barren []string
	for _, term := range q.Terms {
		n, err := s.countMatching(ctx, query.Query{Terms: []query.Term{term}}, schema, tc)
		if err != nil {
			continue
		}
		if n == 0 {
			barren = append(barren, term.String())
		}
	}

	switch {
	case len(barren) == 0 && len(q.Terms) == 1:
		fmt.Fprintf(os.Stderr, "No records matched %s.\n", q.Terms[0].String())
	case len(barren) == 0:
		fmt.Fprintln(os.Stderr, "No records matched. Each term matches something on its own, "+
			"so it is the combination that excludes everything.")
	default:
		fmt.Fprintf(os.Stderr, "No records matched. These terms match nothing on their own: %s\n",
			strings.Join(barren, ", "))
	}
	return nil
}

// overlaps reports whether two intervals share any instant.
func overlaps(a, b query.Interval) bool {
	if !a.Start.IsZero() && !b.End.IsZero() && !a.Start.Before(b.End) {
		return false
	}
	if !b.Start.IsZero() && !a.End.IsZero() && !b.Start.Before(a.End) {
		return false
	}
	return true
}

func (s *session) countMatching(ctx context.Context, q query.Query, schema query.Schema, tc query.TimeContext) (int64, error) {
	resolved, _, err := query.ResolveTime(q, tc)
	if err != nil {
		return 0, err
	}

	sql, err := query.Compile(resolved, schema)
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
