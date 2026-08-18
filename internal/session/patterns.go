package session

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DefaultPatternLimit is how many templates a listing shows when the caller
// does not say.
//
// A real corpus has a long tail of one-off templates — a truncated line is its
// own shape, and correctly so — and the useful ones are all at the top. The
// tail is never dropped silently: what is not shown is counted and stated.
const DefaultPatternLimit = 30

// Pattern is one message template and what is known about it.
type Pattern struct {
	ID       string `json:"id"`
	Template string `json:"template"`
	Count    int64  `json:"count"`

	// First and Last are zero when every record under this template has no
	// timestamp.
	First time.Time `json:"first,omitempty"`
	Last  time.Time `json:"last,omitempty"`

	// NoTimestamp is how many of this template's records carry no timestamp,
	// and so sit outside First and Last entirely.
	NoTimestamp int64 `json:"no_timestamp,omitempty"`

	// Example is one message this template came from, so the masking can be
	// checked against something real.
	Example string   `json:"example"`
	Sources []string `json:"sources"`

	// Before is how many of this template's records fall before a --new-since
	// cutoff. New means none did.
	Before int64 `json:"before,omitempty"`
	New    bool  `json:"new,omitempty"`
}

// PatternQuery configures a listing.
type PatternQuery struct {
	// Limit is how many templates to return, most frequent first. Zero uses
	// DefaultPatternLimit; a negative limit means all of them.
	Limit int

	// NewSince, when non-zero, restricts the listing to templates with no
	// records before the cutoff — the ones that have started happening.
	NewSince time.Duration
}

// PatternSet is a listing plus everything needed to trust it.
type PatternSet struct {
	Patterns []Pattern `json:"patterns"`

	// Templates is how many distinct templates matched the filter, and
	// Records how many records they cover — both before any limit.
	Templates int64 `json:"templates"`
	Records   int64 `json:"records"`

	// Truncated says the limit cut the list. Hidden and HiddenRecords say by
	// how much. A list that stops without saying so is the silent truncation
	// this project refuses.
	Truncated     bool  `json:"truncated"`
	Hidden        int64 `json:"hidden,omitempty"`
	HiddenRecords int64 `json:"hidden_records,omitempty"`

	// Since is the --new-since cutoff and Anchor names what it counted back
	// from. Established is how many templates were left out for having been
	// seen before it.
	Since       time.Time `json:"since,omitempty"`
	Anchor      string    `json:"anchor,omitempty"`
	Established int64     `json:"established,omitempty"`

	// Undated is how many matching records carry no timestamp, and therefore
	// cannot be placed on either side of the cutoff.
	Undated int64 `json:"undated,omitempty"`

	// UnparsedTemplates and UnparsedRecords are the share of the listing that
	// came from lines no parser understood.
	//
	// Worth separating because the two behave nothing alike. Parsed messages
	// collapse hard — a real corpus goes from 185,000 records to 85 templates.
	// A broken line is genuinely its own shape, so 1,500 of them can produce
	// 846 templates and dominate the count. Reporting the split is what stops
	// "931 templates" reading as a failure of the collapse rule.
	UnparsedTemplates int64 `json:"unparsed_templates,omitempty"`
	UnparsedRecords   int64 `json:"unparsed_records,omitempty"`
}

// Patterns groups matching records by message template.
//
// The grouping is a plain GROUP BY on a stored column: the template and its id
// are computed once at ingest, by internal/pattern, so nothing here re-derives
// them and there is no second set of masking rules to disagree with the first.
func (s *Session) Patterns(ctx context.Context, plan Plan, q PatternQuery) (PatternSet, error) {
	out := PatternSet{}

	if err := s.patternTotals(ctx, plan, &out); err != nil {
		return PatternSet{}, err
	}
	if out.Templates == 0 {
		return out, nil
	}

	cutoff, err := s.patternCutoff(ctx, q, &out)
	if err != nil {
		return PatternSet{}, err
	}

	patterns, err := s.patternRows(ctx, plan, q, cutoff)
	if err != nil {
		return PatternSet{}, err
	}

	// --new-since answers "what has started happening", so the established
	// templates are dropped rather than merely marked. How many were dropped
	// is reported, because a filtered list that does not say what it filtered
	// is indistinguishable from a quiet dataset.
	if q.NewSince > 0 {
		kept := patterns[:0]
		for _, p := range patterns {
			if p.New {
				kept = append(kept, p)
				continue
			}
			out.Established++
		}
		patterns = kept
	}

	limit := q.Limit
	if limit == 0 {
		limit = DefaultPatternLimit
	}
	if limit > 0 && len(patterns) > limit {
		for _, p := range patterns[limit:] {
			out.HiddenRecords += p.Count
		}
		out.Hidden = int64(len(patterns) - limit)
		out.Truncated = true
		patterns = patterns[:limit]
	}

	out.Patterns = patterns
	return out, nil
}

// patternTotals counts what matched before any limit is applied, so the
// listing can always say what it is a slice of.
func (s *Session) patternTotals(ctx context.Context, plan Plan, out *PatternSet) error {
	row := s.DB.QueryRow(ctx,
		`SELECT count(DISTINCT pattern_id),
		        count(*),
		        count(*) FILTER (WHERE ts IS NULL),
		        count(DISTINCT pattern_id) FILTER (WHERE NOT parsed),
		        count(*) FILTER (WHERE NOT parsed)
		 FROM logs WHERE `+plan.SQL.Where, plan.SQL.Args...)

	if err := row.Scan(&out.Templates, &out.Records, &out.Undated,
		&out.UnparsedTemplates, &out.UnparsedRecords); err != nil {
		return fmt.Errorf("count templates: %w", err)
	}
	return nil
}

// patternCutoff resolves --new-since to an instant.
//
// It counts back from the same anchor last: uses — the newest record, unless
// the session was opened relative to the wall clock — so "new in the last 15
// minutes" means the same thing here as it does in a filter.
func (s *Session) patternCutoff(ctx context.Context, q PatternQuery, out *PatternSet) (time.Time, error) {
	if q.NewSince <= 0 {
		return time.Time{}, nil
	}

	tc, err := s.TimeContext(ctx)
	if err != nil {
		return time.Time{}, err
	}

	anchor, description := tc.Anchor()
	out.Since = anchor.Add(-q.NewSince)
	out.Anchor = description
	return out.Since, nil
}

// patternRows runs the grouping.
func (s *Session) patternRows(ctx context.Context, plan Plan, q PatternQuery, cutoff time.Time) ([]Pattern, error) {
	// The example is the message the template was derived from, falling back
	// to the raw line for a record no parser understood — the same choice
	// internal/store makes when computing the pattern, so an example can never
	// be text the template was not built from.
	const example = `min(CASE WHEN message <> '' THEN message ELSE raw END)`

	before := "0"
	args := append([]any{}, plan.SQL.Args...)
	if q.NewSince > 0 {
		// Placed before the filter's own arguments would misalign them; DuckDB
		// binds by position, so the order here has to match the statement.
		before = `count(*) FILTER (WHERE ts IS NOT NULL AND ts < ?)`
		args = append([]any{cutoff.UTC()}, args...)
	}

	// Ordered by count, then by id, so the same data always lists in the same
	// order. A golden test over a listing that reordered itself would be worse
	// than no test.
	text := `
		SELECT pattern_id,
		       min(pattern),
		       count(*),
		       min(ts),
		       max(ts),
		       count(*) FILTER (WHERE ts IS NULL),
		       ` + example + `,
		       string_agg(DISTINCT source, chr(31)),
		       ` + before + `
		FROM logs
		WHERE ` + plan.SQL.Where + `
		GROUP BY pattern_id
		ORDER BY count(*) DESC, pattern_id`

	rows, err := s.DB.Query(ctx, text, args...)
	if err != nil {
		return nil, fmt.Errorf("group by template: %w", err)
	}
	defer rows.Close()

	var out []Pattern
	for rows.Next() {
		var (
			p           Pattern
			template    sql.NullString
			first, last sql.NullTime
			exampleText sql.NullString
			sources     sql.NullString
		)

		if err := rows.Scan(&p.ID, &template, &p.Count, &first, &last,
			&p.NoTimestamp, &exampleText, &sources, &p.Before); err != nil {
			return nil, fmt.Errorf("scan template: %w", err)
		}

		p.Template = template.String
		p.Example = exampleText.String
		p.First, p.Last = first.Time, last.Time
		p.Sources = splitSources(sources.String)
		p.New = q.NewSince > 0 && p.Before == 0

		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate templates: %w", err)
	}

	return out, nil
}

// splitSources unpacks the aggregated source names.
//
// A unit separator rather than a comma: a logical source name comes from a
// file name, and nothing stops one containing a comma. Splitting on a character
// that cannot appear in the data is the difference between a list and a guess.
//
// Written as chr(31) in the statement, not as an escape. DuckDB takes a
// single-quoted '\x1f' literally, four characters of it, so the join and this
// split would have agreed on nothing and every multi-source template would
// have reported one run-on name.
func splitSources(joined string) []string {
	if joined == "" {
		return nil
	}

	out := strings.Split(joined, "\x1f")
	// string_agg does not promise an order, and a listing whose sources
	// reordered between runs could not be golden-tested.
	sort.Strings(out)
	return out
}
