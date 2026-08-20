package session

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/GrantPukka/loupe/internal/query"
)

// DefaultDiffLimit is how many differences a comparison shows when the caller
// does not say.
//
// The ranking is the feature, so the head of the list is where the answer is.
// What the limit cut is counted and stated, never dropped quietly.
const DefaultDiffLimit = 20

// DiffKind is what sort of thing differs.
type DiffKind string

const (
	// DiffPattern is a message template, as `loupe patterns` computes them.
	DiffPattern DiffKind = "pattern"
	// DiffField is a field name, compared on how many records carry it.
	DiffField DiffKind = "field"
	// DiffValue is one value of one field.
	DiffValue DiffKind = "value"
)

// diffKinds is the order the kinds are reported in, coarsest first.
var diffKinds = []DiffKind{DiffPattern, DiffField, DiffValue}

// Plural names a kind for a footer that has to count them.
func (k DiffKind) Plural() string {
	switch k {
	case DiffPattern:
		return "templates"
	case DiffField:
		return "fields"
	default:
		return "field values"
	}
}

// DiffChange says how an item differs between the two windows.
type DiffChange string

const (
	// DiffAppeared is present after and absent before.
	DiffAppeared DiffChange = "appeared"
	// DiffVanished is present before and absent after.
	DiffVanished DiffChange = "vanished"
	// DiffShifted is present in both, at a different rate.
	DiffShifted DiffChange = "shifted"
)

// DiffWindow is one side of a comparison.
type DiffWindow struct {
	// Expr is the window as the user wrote it.
	Expr string `json:"expr"`
	// Filter is the full filter the window was resolved as, which is Expr
	// intersected with any filter given alongside it.
	Filter string `json:"filter"`

	// Interval is the resolved window, and Records how many records fell in it.
	Interval query.Interval `json:"-"`
	Records  int64          `json:"records"`

	// Notes are the resolver's own notes about how the window was read: which
	// day a bare time landed on, a clock change inside it, an unbounded side
	// clamped to the data.
	Notes []string `json:"notes,omitempty"`
}

// Duration is the length of the window.
func (w DiffWindow) Duration() time.Duration { return w.Interval.Duration() }

// DiffItem is one thing that differs between the windows.
type DiffItem struct {
	Kind DiffKind `json:"kind"`
	// Key is the item's handle: a template id for a pattern.
	Key string `json:"key"`
	// Label is the item rendered for a person, and Detail the longer text
	// behind it — a template's masked message.
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`

	// Before and After are the counts in each window, and BeforeRate and
	// AfterRate those counts per second.
	Before     int64   `json:"before"`
	After      int64   `json:"after"`
	BeforeRate float64 `json:"before_rate"`
	AfterRate  float64 `json:"after_rate"`

	Change DiffChange `json:"change"`

	// Surprise ranks the item. See surprise.
	Surprise float64 `json:"surprise"`
}

// RateChange is the fractional change in rate, from -1 (gone) upward, and
// false when there was no rate to change from.
func (i DiffItem) RateChange() (float64, bool) {
	if i.BeforeRate <= 0 {
		return 0, false
	}
	return (i.AfterRate - i.BeforeRate) / i.BeforeRate, true
}

// DiffCount is how much of one kind was looked at and how much of it differed.
type DiffCount struct {
	Kind     DiffKind `json:"kind"`
	Compared int64    `json:"compared"`
	Differed int64    `json:"differed"`
}

// DiffSkipped is a field whose values were too many to compare one by one.
type DiffSkipped struct {
	Field    string `json:"field"`
	Distinct int64  `json:"distinct"`
}

// DiffQuery configures a comparison.
type DiffQuery struct {
	// Before and After are the two windows, written in the filter language's
	// own time grammar: 13:00-14:00, last:15m, on:2026-08-13.
	Before, After string

	// Filter is applied to both windows, so a comparison can be narrowed to
	// one source without writing it twice.
	Filter string

	// Limit is how many differences to return. Zero uses DefaultDiffLimit; a
	// negative limit means all of them.
	Limit int
}

// DiffSet is a comparison and everything needed to trust it.
type DiffSet struct {
	Before DiffWindow `json:"before"`
	After  DiffWindow `json:"after"`

	// Items are the differences, most surprising first.
	Items []DiffItem `json:"items"`

	// Compared is how many distinct things were looked at, and Differed how
	// many of them actually changed. The gap is the answer to "is this quiet
	// or did I ask the wrong question".
	Compared int64 `json:"compared"`
	Differed int64 `json:"differed"`

	// Counts breaks those two totals down by kind, because the denominators
	// are nothing alike: forty templates and forty thousand field values are
	// both "compared", and a single number hides which one the answer came
	// from.
	Counts []DiffCount `json:"counts"`

	// Skipped names the fields whose values were not compared one by one, and
	// how many values each has.
	Skipped []DiffSkipped `json:"skipped,omitempty"`

	// Unnameable names the fields left out entirely because their names cannot
	// be written into a query. See referenceable.
	Unnameable []string `json:"unnameable,omitempty"`

	// Rates says the windows are of unequal length, so counts are not
	// comparable and the ranking is over rates.
	Rates bool `json:"rates"`

	// NoTimestamp is how many records the filter matched that carry no
	// timestamp, and so fall in neither window.
	NoTimestamp int64 `json:"no_timestamp,omitempty"`

	// Overlap is the intersection of the two windows when they overlap. A
	// record inside it is counted on both sides.
	Overlap query.Interval `json:"-"`

	// Truncated says the limit cut the list, and Hidden by how much.
	Truncated bool  `json:"truncated"`
	Hidden    int64 `json:"hidden,omitempty"`
}

// Diff compares two time windows and reports what is different about them.
//
// The question is "what is different between the healthy window and the
// incident window", which no grep workflow answers. Both sides go through the
// ordinary filter path, so each window's numbers are exactly what `loupe
// <dir> '<window>'` would report for it — a comparison whose halves disagreed
// with the tool's own listings would be worse than no comparison.
func (s *Session) Diff(ctx context.Context, q DiffQuery) (DiffSet, error) {
	if strings.TrimSpace(q.Before) == "" || strings.TrimSpace(q.After) == "" {
		return DiffSet{}, fmt.Errorf("a comparison needs two windows, e.g. " +
			"--before 13:00-14:00 --after 14:00-15:00")
	}

	before, beforePlan, err := s.diffWindow(ctx, q.Filter, q.Before, "--before")
	if err != nil {
		return DiffSet{}, err
	}
	after, afterPlan, err := s.diffWindow(ctx, q.Filter, q.After, "--after")
	if err != nil {
		return DiffSet{}, err
	}

	out := DiffSet{
		Before:  before,
		After:   after,
		Rates:   unequalLengths(before.Duration(), after.Duration()),
		Overlap: overlapOf(before.Interval, after.Interval),
	}

	compared := map[DiffKind]int64{}

	patterns, templates, err := s.diffPatterns(ctx, beforePlan, afterPlan)
	if err != nil {
		return DiffSet{}, err
	}
	compared[DiffPattern] = templates

	fields, err := s.diffFieldsAndValues(ctx, beforePlan, afterPlan, &out, compared)
	if err != nil {
		return DiffSet{}, err
	}

	// A record with no timestamp is in neither window. It is not lost — ts:none
	// still finds it — but a comparison of two time windows has nowhere to put
	// it, so the count is stated rather than left to be inferred.
	if out.NoTimestamp, err = s.diffUndated(ctx, q.Filter); err != nil {
		return DiffSet{}, err
	}

	// Scored once over everything, so a template and a field value are ranked
	// on the same scale and the head of the list is the answer whatever kind
	// it turns out to be.
	items := scoreDiff(append(patterns, fields...), before, after)
	out.Items = rankDiff(items)
	out.Differed = int64(len(out.Items))
	out.summarise(compared)
	out.truncate(q.Limit)

	return out, nil
}

// truncate applies the caller's limit, keeping the real count in Differed so
// the report can always say what it left out.
func (set *DiffSet) truncate(limit int) {
	if limit == 0 {
		limit = DefaultDiffLimit
	}
	if limit <= 0 || len(set.Items) <= limit {
		return
	}

	set.Hidden = int64(len(set.Items) - limit)
	set.Truncated = true
	set.Items = set.Items[:limit]
}

// diffWindow resolves one side.
//
// The window is a filter expression, so it goes through Plan like everything
// else: a bare 14:00 lands on a day the data covers, a clock change inside the
// window is noted, and the filter given alongside intersects with it exactly as
// two written terms would.
func (s *Session) diffWindow(ctx context.Context, filter, window, flag string) (DiffWindow, Plan, error) {
	expr := strings.TrimSpace(strings.TrimSpace(filter) + " " + strings.TrimSpace(window))

	plan, err := s.Plan(ctx, expr)
	if err != nil {
		return DiffWindow{}, Plan{}, fmt.Errorf("%s %q: %w", flag, window, err)
	}

	if !plan.Resolution.HasTimeFilter() {
		return DiffWindow{}, Plan{}, fmt.Errorf(
			"%s %q does not name a time window\n"+
				"windows are written in the filter language's own time grammar, "+
				"e.g. %s 13:00-14:00, %s last:15m, or %s on:2026-08-13",
			flag, window, flag, flag, flag)
	}

	out := DiffWindow{Expr: window, Filter: expr, Interval: plan.Resolution.Interval}
	for _, note := range plan.Resolution.Notes {
		out.Notes = append(out.Notes, note.Text)
	}

	// An open-ended window has no duration, and a rate needs one. Clamping to
	// the span the data actually covers is the only bound that means anything,
	// and it is disclosed rather than assumed: the resolved window is printed
	// in both zones before any result.
	clamped, note, err := s.clampWindow(ctx, out.Interval)
	if err != nil {
		return DiffWindow{}, Plan{}, err
	}
	out.Interval = clamped
	if note != "" {
		out.Notes = append(out.Notes, note)
	}

	if out.Records, err = s.Count(ctx, plan); err != nil {
		return DiffWindow{}, Plan{}, err
	}
	return out, plan, nil
}

// clampWindow bounds an open-ended window by the data's own range.
func (s *Session) clampWindow(ctx context.Context, in query.Interval) (query.Interval, string, error) {
	if !in.Start.IsZero() && !in.End.IsZero() {
		return in, "", nil
	}

	tc, err := s.TimeContext(ctx)
	if err != nil {
		return in, "", err
	}

	out := in
	side := ""
	if out.Start.IsZero() && !tc.Oldest.IsZero() {
		out.Start, side = tc.Oldest, "start"
	}
	if out.End.IsZero() && !tc.Newest.IsZero() {
		// The newest record has to fall inside the window, and an interval is
		// half-open.
		out.End = tc.Newest.Add(time.Nanosecond)
		if side == "" {
			side = "end"
		} else {
			side = "both ends"
		}
	}

	if side == "" {
		return out, "", nil
	}
	return out, fmt.Sprintf(
		"the window is open at its %s, so it was bounded by the data's own range "+
			"to give it a length to compare", side), nil
}

// diffUndated counts the records that carry no timestamp.
func (s *Session) diffUndated(ctx context.Context, filter string) (int64, error) {
	plan, err := s.Plan(ctx, strings.TrimSpace(filter+" ts:none"))
	if err != nil {
		return 0, err
	}
	return s.Count(ctx, plan)
}

// diffPatterns compares the message templates in the two windows.
//
// Both sides come from Session.Patterns, so the templates are EC002's — the
// ones computed once at ingest — and there is no second set of masking rules
// here to disagree with the first.
func (s *Session) diffPatterns(ctx context.Context, before, after Plan) ([]DiffItem, int64, error) {
	beforeSet, err := s.Patterns(ctx, before, PatternQuery{Limit: -1})
	if err != nil {
		return nil, 0, fmt.Errorf("templates in the before window: %w", err)
	}
	afterSet, err := s.Patterns(ctx, after, PatternQuery{Limit: -1})
	if err != nil {
		return nil, 0, fmt.Errorf("templates in the after window: %w", err)
	}

	index := map[string]*DiffItem{}
	item := func(p Pattern) *DiffItem {
		got, ok := index[p.ID]
		if !ok {
			got = &DiffItem{Kind: DiffPattern, Key: p.ID, Label: "pattern " + p.ID}
			index[p.ID] = got
		}
		// A template that occurs in both windows has the same text in both, so
		// whichever side fills it in first is the same string.
		if got.Detail == "" {
			got.Detail = p.Template
		}
		return got
	}

	for _, p := range beforeSet.Patterns {
		item(p).Before = p.Count
	}
	for _, p := range afterSet.Patterns {
		item(p).After = p.Count
	}

	// Collected in id order so the comparison is deterministic before it is
	// ranked: two items with the same surprise must not swap between runs.
	ids := make([]string, 0, len(index))
	for id := range index {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]DiffItem, 0, len(ids))
	for _, id := range ids {
		out = append(out, *index[id])
	}
	return out, int64(len(ids)), nil
}

// summarise fills in the per-kind tallies once every item has been scored.
func (set *DiffSet) summarise(compared map[DiffKind]int64) {
	differed := map[DiffKind]int64{}
	for _, item := range set.Items {
		differed[item.Kind]++
	}

	for _, kind := range diffKinds {
		if compared[kind] == 0 {
			continue
		}
		set.Counts = append(set.Counts, DiffCount{
			Kind:     kind,
			Compared: compared[kind],
			Differed: differed[kind],
		})
		set.Compared += compared[kind]
	}
}

// scoreDiff fills in the rates, the change, and the ranking statistic, and
// drops the items that did not change at all.
//
// Rates come from the windows' durations, because that is what a reader
// comparing five minutes against an hour needs to see. The ranking comes from
// the windows' record counts, because that is what separates a real change from
// everything moving together. The two are different questions and the table
// answers both.
func scoreDiff(items []DiffItem, before, after DiffWindow) []DiffItem {
	out := items[:0]

	for _, item := range items {
		item.BeforeRate = ratePerSecond(item.Before, before.Duration())
		item.AfterRate = ratePerSecond(item.After, after.Duration())
		item.Surprise = surprise(item.Before, item.After, before.Records, after.Records)

		switch {
		case item.Before == 0 && item.After == 0:
			continue
		case item.Before == 0:
			item.Change = DiffAppeared
		case item.After == 0:
			item.Change = DiffVanished
		default:
			item.Change = DiffShifted
		}

		// A surprise of zero means the item occurred at exactly the rate the
		// other window would predict. That is not a difference, and listing it
		// would bury the ones that are.
		if item.Surprise <= 0 {
			continue
		}
		out = append(out, item)
	}
	return out
}

// rankDiff orders the differences, most surprising first.
func rankDiff(items []DiffItem) []DiffItem {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Surprise != items[j].Surprise {
			return items[i].Surprise > items[j].Surprise
		}
		// A stable tiebreak on the handle, so the same data always lists in the
		// same order and one run can be compared against another.
		return items[i].Key < items[j].Key
	})
	return items
}

// unequalLengths reports whether two windows differ in length by enough to
// matter.
//
// Compared to the second, not to the nanosecond. A window bounded by the newest
// record is one nanosecond longer than the wall-clock span it names, and letting
// that flip the whole table from counts to rates — over a difference no reader
// could see, and which would print as two identical lengths — would be the
// clamp deciding how the report renders.
func unequalLengths(before, after time.Duration) bool {
	return before.Round(time.Second) != after.Round(time.Second)
}

func ratePerSecond(n int64, over time.Duration) float64 {
	if over <= 0 {
		return 0
	}
	return float64(n) / over.Seconds()
}

// surprise scores how unexpected an item's change is.
//
// This is the log-likelihood ratio — the G² statistic — against the hypothesis
// that the item makes up the same share of both windows. Expected counts are
// apportioned by how many records each window holds, so the score answers
// "what is different about this window beyond there simply being more of it".
//
// Apportioning by *duration* instead was the obvious first choice and it is
// wrong here: when traffic goes up sixty-fold, so does every field that every
// record carries, and the top of the list fills with `field level ×61`, `field
// path ×61`, `field status ×61` — sixty-one restated once per field, burying
// the one template that actually appeared. The change in volume is a single
// fact, and it is printed once above the table rather than once per row.
//
// The checklist item this satisfies is "rank by most surprising, not raw
// delta". A raw delta cannot tell 2 → 4 from 0 → 300; between windows of equal
// size the first scores 0.68 and the second 416, because a doubling of two is
// exactly what noise looks like and an appearance out of nothing is not. A
// large drop scores highest of all: 9,800 → 480 is 10,366, which is right,
// because a service that stopped saying what it always said is the most
// informative thing in either window.
func surprise(before, after int64, beforeOf, afterOf int64) float64 {
	total := before + after
	if total == 0 {
		return 0
	}

	// With nothing in one window there is no share to compare against, and
	// every item would score zero. Diff reports that case in words instead.
	if beforeOf <= 0 || afterOf <= 0 {
		return 0
	}

	scale := float64(beforeOf) + float64(afterOf)
	expectedBefore := float64(total) * float64(beforeOf) / scale
	expectedAfter := float64(total) * float64(afterOf) / scale

	return 2 * (term(float64(before), expectedBefore) + term(float64(after), expectedAfter))
}

// term is one half of the log-likelihood ratio, with the 0 log 0 = 0
// convention that the statistic is defined under.
func term(observed, expected float64) float64 {
	if observed <= 0 || expected <= 0 {
		return 0
	}
	return observed * math.Log(observed/expected)
}

// overlapOf returns the intersection of two windows, or an empty interval when
// they do not overlap.
//
// Overlapping windows are not refused: a record inside the overlap is counted
// on both sides, which is what makes each side's numbers match what the tool
// would print for that window on its own. It is reported, because a comparison
// of a window with itself would otherwise look like a comparison of two things.
func overlapOf(a, b query.Interval) query.Interval {
	out := a.Intersect(b)
	if out.Empty() {
		return query.Interval{}
	}
	return out
}
