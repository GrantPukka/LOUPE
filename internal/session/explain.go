package session

import (
	"context"
	"fmt"
	"time"

	"github.com/VIGIL-OPS/loupe/internal/query"
)

// Explanation says why a filter matched nothing.
//
// An empty table with no explanation is the most misleading output this tool
// can produce: the user cannot tell whether their filter was wrong or their
// logs genuinely contain nothing. Both front ends need this, so it is computed
// here and rendered by the caller.
type Explanation struct {
	// Text is a one-line summary suitable for a status line.
	Text string `json:"text"`

	// OutsideWindow is set when the filter's window misses the data entirely,
	// which is both the commonest cause and the most fixable.
	OutsideWindow bool `json:"outside_window"`
	// DataStart and DataEnd bound what the data actually covers.
	DataStart time.Time `json:"data_start,omitempty"`
	DataEnd   time.Time `json:"data_end,omitempty"`

	// BarrenTerms are the terms that match nothing even on their own. A term
	// that matches plenty alone is only guilty in combination.
	BarrenTerms []string `json:"barren_terms,omitempty"`
}

// Explain works out why a plan returned nothing.
func (s *Session) Explain(ctx context.Context, plan Plan) Explanation {
	tc, err := s.TimeContext(ctx)
	if err != nil {
		return Explanation{Text: "No records matched."}
	}

	if plan.Resolution.HasTimeFilter() && !tc.Oldest.IsZero() {
		data := query.Interval{Start: tc.Oldest, End: tc.Newest.Add(time.Nanosecond)}
		if !overlaps(plan.Resolution.Interval, data) {
			return Explanation{
				Text: fmt.Sprintf("No records in that window. The data covers %s.",
					data.Describe(s.Loc)),
				OutsideWindow: true,
				DataStart:     tc.Oldest,
				DataEnd:       tc.Newest,
			}
		}
	}

	sch, err := s.Schema(ctx)
	if err != nil {
		return Explanation{Text: "No records matched."}
	}

	var barren []string
	for _, term := range plan.Query.Terms {
		single := query.Query{Terms: []query.Term{term}}

		resolved, _, err := query.ResolveTime(single, tc)
		if err != nil {
			continue
		}
		compiled, err := query.Compile(resolved, sch)
		if err != nil {
			continue
		}

		var n int64
		row := s.DB.QueryRow(ctx, `SELECT count(*) FROM logs WHERE `+compiled.Where, compiled.Args...)
		if err := row.Scan(&n); err != nil {
			continue
		}
		if n == 0 {
			barren = append(barren, term.String())
		}
	}

	switch {
	case len(barren) == 0 && len(plan.Query.Terms) == 1:
		return Explanation{Text: fmt.Sprintf("No records matched %s.", plan.Query.Terms[0])}
	case len(barren) == 0:
		return Explanation{Text: "No records matched. Each term matches something on its own, " +
			"so it is the combination that excludes everything."}
	default:
		return Explanation{
			Text: fmt.Sprintf("No records matched. These terms match nothing on their own: %s",
				joinTerms(barren)),
			BarrenTerms: barren,
		}
	}
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

func joinTerms(terms []string) string {
	out := ""
	for i, t := range terms {
		if i > 0 {
			out += ", "
		}
		out += t
	}
	return out
}
