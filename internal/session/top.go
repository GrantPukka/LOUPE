package session

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/GrantPukka/loupe/internal/query"
)

// DefaultTopLimit is how many values a breakdown shows when the caller does not
// say. Twenty fits a terminal and covers the head of almost any distribution;
// what it leaves out is counted and stated, never dropped quietly.
const DefaultTopLimit = 20

// TopValue is one value of a field and how often it occurs.
type TopValue struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
	// Share is the fraction of records carrying the field that hold this
	// value, from 0 to 1. Rendered as a percentage by the caller.
	Share float64 `json:"share"`
}

// TopQuery configures a breakdown.
type TopQuery struct {
	// Field is the field to group by. Required.
	Field string
	// Limit is how many values to return, most frequent first. Zero uses
	// DefaultTopLimit; a negative limit means all of them.
	Limit int
}

// TopSet is a value breakdown plus everything needed to trust it.
type TopSet struct {
	Field  string     `json:"field"`
	Values []TopValue `json:"values"`

	// Distinct is how many distinct values matched, before any limit.
	Distinct int64 `json:"distinct"`

	// Matched is how many records the filter matched at all, and Present how
	// many of those carry a value for this field.
	Matched int64 `json:"matched"`
	Present int64 `json:"present"`

	// Absent is Matched minus Present: records the filter matched that have no
	// value for this field at all.
	//
	// Stated rather than quietly excluded. "412 of 500 requests hit
	// /api/checkout" means something different if 88 of those 500 records have
	// no path field — the reader is entitled to know the breakdown covers less
	// than the filter matched.
	Absent int64 `json:"absent,omitempty"`

	// Truncated says the limit cut the list, and Hidden how much it cut. A
	// list that stops without saying so understates the data.
	Truncated     bool  `json:"truncated"`
	Hidden        int64 `json:"hidden,omitempty"`
	HiddenRecords int64 `json:"hidden_records,omitempty"`
}

// Top counts the values of one field among matching records, most frequent
// first.
//
// This is the most common triage question — "which endpoints are 500ing?" —
// and a GROUP BY DuckDB does for free. It exists so nobody has to drop to
// `loupe sql` to ask it.
func (s *Session) Top(ctx context.Context, plan Plan, q TopQuery) (TopSet, error) {
	if q.Field == "" {
		return TopSet{}, fmt.Errorf("name a field to break down, e.g. `loupe top path`")
	}

	sch, err := s.Schema(ctx)
	if err != nil {
		return TopSet{}, err
	}

	expr, err := topExprFor(sch, q.Field)
	if err != nil {
		return TopSet{}, err
	}

	out := TopSet{Field: q.Field}
	if err := s.topTotals(ctx, plan, expr, &out); err != nil {
		return TopSet{}, err
	}
	if out.Present == 0 {
		return out, nil
	}

	values, err := s.topValues(ctx, plan, expr)
	if err != nil {
		return TopSet{}, err
	}

	limit := q.Limit
	if limit == 0 {
		limit = DefaultTopLimit
	}
	if limit > 0 && len(values) > limit {
		for _, v := range values[limit:] {
			out.HiddenRecords += v.Count
		}
		out.Hidden = int64(len(values) - limit)
		out.Truncated = true
		values = values[:limit]
	}

	// The share is of the records that carry the field, so the values sum to
	// one and the breakdown reads as a distribution. Absent records are
	// reported separately rather than folded into the denominator, where they
	// would silently shrink every percentage.
	for i := range values {
		values[i].Share = float64(values[i].Count) / float64(out.Present)
	}

	out.Values = values
	return out, nil
}

// topTotals counts what matched, what carries the field, and how many distinct
// values there are — all before any limit, so the listing can always say what
// it is a slice of.
func (s *Session) topTotals(ctx context.Context, plan Plan, expr topExpr, out *TopSet) error {
	// Argument order follows the order the placeholders appear in the
	// statement, which is what DuckDB binds by: the expression twice, then the
	// filter.
	args := concatArgs(expr.Args, expr.Args, plan.SQL.Args)

	row := s.DB.QueryRow(ctx,
		`SELECT count(*),
		        count(*) FILTER (WHERE (`+expr.SQL+`) IS NOT NULL),
		        count(DISTINCT `+expr.SQL+`)
		 FROM logs WHERE `+plan.SQL.Where, args...)

	if err := row.Scan(&out.Matched, &out.Present, &out.Distinct); err != nil {
		return fmt.Errorf("count %s values: %w", out.Field, err)
	}
	out.Absent = out.Matched - out.Present
	return nil
}

// topValues groups the matching records by the field's value.
func (s *Session) topValues(ctx context.Context, plan Plan, expr topExpr) ([]TopValue, error) {
	args := concatArgs(expr.Args, plan.SQL.Args, expr.Args)

	// Ordered by count, then by value, so the same data always lists in the
	// same order. A breakdown that reordered itself between runs could not be
	// compared against one taken a minute earlier.
	rows, err := s.DB.Query(ctx,
		`SELECT (`+expr.SQL+`) AS value, count(*) AS n
		 FROM logs
		 WHERE (`+plan.SQL.Where+`) AND (`+expr.SQL+`) IS NOT NULL
		 GROUP BY 1
		 ORDER BY n DESC, 1`, args...)
	if err != nil {
		return nil, fmt.Errorf("group by %s: %w", expr.SQL, err)
	}
	defer rows.Close()

	var out []TopValue
	for rows.Next() {
		var (
			value sql.NullString
			n     int64
		)
		if err := rows.Scan(&value, &n); err != nil {
			return nil, fmt.Errorf("scan value: %w", err)
		}
		out = append(out, TopValue{Value: value.String, Count: n})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate values: %w", err)
	}
	return out, nil
}

// topExpr is the SQL that produces the value being counted, with any parameters
// it carries of its own.
//
// A column name resolves to a fixed identifier and needs none. A regex is user
// text and must be a parameter, for the reason CLAUDE.md gives about never
// building SQL by concatenation however small the change looks.
type topExpr struct {
	SQL  string
	Args []any
}

// concatArgs joins argument lists in placeholder order.
func concatArgs(lists ...[]any) []any {
	var out []any
	for _, list := range lists {
		out = append(out, list...)
	}
	return out
}

// topExprFor resolves what to break down: a field, or a regex capture.
//
// The regex form exists because the interesting value is not always a field.
// sshd writes "Failed password for root" and "Failed password for invalid user
// root", and with the username inside unparsed text there is no field to group
// by — which left hand-written SQL as the only way to ask the question `top`
// exists to answer. It is spelled the way the filter language spells a regex,
// so there is nothing new to learn:
//
//	loupe top '/Failed password for (?:invalid user )?(\S+)/'
//	loupe top 'message~/GET (/[^ ?]*)/'
func topExprFor(sch query.Schema, field string) (topExpr, error) {
	target, pattern, ok := splitRegexTarget(field)
	if !ok {
		// The same resolver the filter compiler uses, so a facet and a filter
		// agree about which column a name means — and an unknown name gets the
		// same spelling suggestion here as anywhere else, rather than an empty
		// table.
		expr, err := sch.Column(field)
		if err != nil {
			return topExpr{}, err
		}
		return topExpr{SQL: expr}, nil
	}

	// Compiled here as well as in DuckDB, so a bad pattern is a clear error
	// before a query runs, and so the capture count can be read off it.
	re, err := regexp.Compile(pattern)
	if err != nil {
		return topExpr{}, fmt.Errorf("invalid regex %q: %w", pattern, err)
	}

	column := "raw"
	if target != "" {
		if column, err = sch.Column(target); err != nil {
			return topExpr{}, err
		}
	}

	// Group 1 when the pattern captures, the whole match otherwise, which is
	// what someone writing /ERROR \w+/ means. NULLIF because regexp_extract
	// returns an empty string for a row that did not match, and an empty string
	// would otherwise be counted as a value in its own right.
	group := 0
	if re.NumSubexp() > 0 {
		group = 1
	}

	return topExpr{
		SQL:  fmt.Sprintf("NULLIF(regexp_extract(%s, ?, %d), '')", column, group),
		Args: []any{pattern},
	}, nil
}

// splitRegexTarget reads the `/pattern/` and `field~/pattern/` forms.
func splitRegexTarget(field string) (target, pattern string, ok bool) {
	if at := strings.Index(field, "~"); at >= 0 {
		target, field = field[:at], field[at+1:]
	}
	if len(field) < 2 || !strings.HasPrefix(field, "/") || !strings.HasSuffix(field, "/") {
		return "", "", false
	}
	return target, field[1 : len(field)-1], true
}
