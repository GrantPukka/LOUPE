package query

import (
	"fmt"
	"strings"
	"time"
)

// TimeContext is everything needed to turn a written time into an instant.
//
// It is supplied by the caller rather than discovered here, so that resolution
// is a pure function of the query and the data's shape, and can be tested
// without a database or a clock.
type TimeContext struct {
	// Loc is the display timezone. Bare times in the query are interpreted in
	// it, and results are shown in it.
	Loc *time.Location

	// Oldest and Newest bound the loaded data. Bare times resolve against this
	// range, and last: anchors to Newest.
	Oldest, Newest time.Time

	// Now is wall-clock time, used only when RelativeToNow is set.
	Now time.Time

	// RelativeToNow makes last: measure back from the wall clock instead of the
	// newest record. It is what --relative-to=now and --follow set.
	//
	// The default is deliberately the other way round: last:15m against a log
	// file from yesterday returning nothing is the single most confusing
	// possible result.
	RelativeToNow bool
}

func (tc TimeContext) location() *time.Location {
	if tc.Loc == nil {
		return time.UTC
	}
	return tc.Loc
}

// anchor is the instant last: counts back from.
func (tc TimeContext) anchor() (time.Time, string) {
	if tc.RelativeToNow || tc.Newest.IsZero() {
		now := tc.Now
		if now.IsZero() {
			now = time.Now()
		}
		return now, "wall clock"
	}
	return tc.Newest, "the newest record"
}

// Note is something the user must be told about how their query was
// interpreted.
//
// These are not warnings in the ignorable sense. Each one records a decision
// the tool made on the user's behalf that changes which records they see, and
// docs/FILTER-DSL.md requires every one of them be surfaced.
type Note struct {
	Kind NoteKind
	Text string
}

// NoteKind classifies a note so callers can style or filter them.
type NoteKind int

const (
	// NoteResolvedDate records which calendar day a bare time was placed on.
	NoteResolvedDate NoteKind = iota
	// NoteAnchor records what last: measured back from.
	NoteAnchor
	// NoteDST records a clock change inside the window.
	NoteDST
	// NoteSkippedTime records a local time that never happened.
	NoteSkippedTime
	// NoteRepeatedTime records a local time that happened twice.
	NoteRepeatedTime
)

// Resolution is the outcome of resolving a query's time terms.
type Resolution struct {
	// Interval is every time term intersected into one window.
	Interval Interval
	// Exclude holds windows removed by negated time terms.
	Exclude []Interval
	Notes   []Note
}

// HasTimeFilter reports whether any time constraint was applied, which is what
// decides whether the excluded-for-no-timestamp count must be reported.
func (r Resolution) HasTimeFilter() bool {
	return !r.Interval.Unbounded() || len(r.Exclude) > 0
}

func (r *Resolution) note(kind NoteKind, format string, args ...any) {
	r.Notes = append(r.Notes, Note{Kind: kind, Text: fmt.Sprintf(format, args...)})
}

// ResolveTime replaces a query's time terms with a single resolved interval.
//
// docs/FILTER-DSL.md section 9 requires this happen at AST level, before
// compiling, so that overlapping terms intersect into one index-friendly
// predicate rather than producing a pile of redundant comparisons.
func ResolveTime(q Query, tc TimeContext) (Query, Resolution, error) {
	res := Resolution{}
	out := Query{Terms: make([]Term, 0, len(q.Terms))}

	var resolved *ResolvedTimeTerm

	for _, term := range q.Terms {
		tt, ok := term.(*TimeTerm)
		if !ok {
			out.Terms = append(out.Terms, term)
			continue
		}

		interval, err := resolveTerm(tt, tc, &res)
		if err != nil {
			return Query{}, Resolution{}, err
		}

		if resolved == nil {
			resolved = &ResolvedTimeTerm{}
			out.Terms = append(out.Terms, resolved)
		}

		if tt.Negate {
			resolved.Exclude = append(resolved.Exclude, interval)
			res.Exclude = append(res.Exclude, interval)
			continue
		}
		resolved.Interval = resolved.Interval.Intersect(interval)
	}

	if resolved == nil {
		return out, res, nil
	}

	res.Interval = resolved.Interval

	// A window that crosses a clock change is not the duration it appears to
	// be. Report it rather than letting the user do arithmetic that is wrong.
	for _, tr := range findTransitions(res.Interval, tc.location()) {
		res.note(NoteDST, "%s. The window spans %s of real time, not the %s it appears to.",
			tr.String(), humanDuration(res.Interval.Duration()), humanDuration(apparentDuration(res.Interval, tc.location())))
	}

	return out, res, nil
}

// resolveTerm turns one written time term into an interval.
func resolveTerm(t *TimeTerm, tc TimeContext, res *Resolution) (Interval, error) {
	loc := tc.location()

	switch t.Keyword {
	case "last":
		return resolveLast(t.Expr, tc, res)

	case "on":
		return resolveOn(t.Expr, loc, res)

	case "after", "since":
		start, err := resolveBoundary(t.Expr, tc, res, boundaryStart)
		if err != nil {
			return Interval{}, err
		}
		return Interval{Start: start}, nil

	case "before", "until":
		end, err := resolveBoundary(t.Expr, tc, res, boundaryEnd)
		if err != nil {
			return Interval{}, err
		}
		return Interval{End: end}, nil

	case "between", "":
		return resolveRange(t.Expr, tc, res)

	default:
		return Interval{}, &TimeError{Expr: t.String(), Reason: "unknown time keyword"}
	}
}

func resolveLast(expr string, tc TimeContext, res *Resolution) (Interval, error) {
	d, err := parseDuration(expr)
	if err != nil {
		return Interval{}, &TimeError{
			Expr:     "last:" + expr,
			Reason:   err.Error(),
			Examples: []string{"last:15m", "last:2h", "last:3d"},
		}
	}

	anchor, description := tc.anchor()
	if anchor.IsZero() {
		return Interval{}, &TimeError{
			Expr:   "last:" + expr,
			Reason: "no records carry a timestamp, so there is nothing to measure back from",
		}
	}

	// The end is exclusive everywhere else, so nudge past the anchor to keep
	// the newest record itself inside its own last: window.
	end := anchor.Add(time.Nanosecond)

	res.note(NoteAnchor, "last:%s is relative to %s (%s), not to the current time.",
		expr, description, formatLocal(anchor, tc.location()))

	return Interval{Start: anchor.Add(-d), End: end}, nil
}

func resolveOn(expr string, loc *time.Location, res *Resolution) (Interval, error) {
	w, err := parseWallTime(expr)
	if err != nil || !w.HasDate {
		return Interval{}, &TimeError{
			Expr:     "on:" + expr,
			Reason:   "not a calendar date",
			Examples: []string{"on:2026-08-13"},
		}
	}

	start := resolveInstant(w, w.Year, w.Month, w.Day, 0, 0, 0, loc, res, "on:"+expr, boundaryStart)
	// The whole day, half-open: the next day's midnight is the exclusive end.
	// Adding 24 hours would be wrong on a day that is 23 or 25 hours long.
	next := time.Date(w.Year, w.Month, w.Day, 0, 0, 0, 0, loc).AddDate(0, 0, 1)

	return Interval{Start: start, End: next}, nil
}

type boundaryKind int

const (
	boundaryStart boundaryKind = iota
	boundaryEnd
)

// resolveBoundary resolves one edge of a window.
func resolveBoundary(expr string, tc TimeContext, res *Resolution, kind boundaryKind) (time.Time, error) {
	loc := tc.location()

	w, err := parseWallTime(expr)
	if err != nil {
		return time.Time{}, &TimeError{
			Expr:     expr,
			Reason:   err.Error(),
			Examples: []string{"14:00", "2:00pm", "2026-08-13T14:00:00Z"},
		}
	}

	// An explicit offset wins over the display timezone, per section 2.3.
	if w.HasZone {
		return w.Instant, nil
	}

	year, month, day, err := datePart(w, tc, res, expr)
	if err != nil {
		return time.Time{}, err
	}

	if !w.HasClock {
		// A bare date as a boundary means the start of that day, or for an
		// exclusive end, the start of the next one — so that before:2026-08-13
		// excludes the 13th rather than half of it.
		start := time.Date(year, month, day, 0, 0, 0, 0, loc)
		if kind == boundaryEnd {
			return start.AddDate(0, 0, 1), nil
		}
		return start, nil
	}

	return resolveInstant(w, year, month, day, w.Hour, w.Minute, w.Second, loc, res, expr, kind), nil
}

// resolveInstant places a wall clock on the timeline, reporting the clock-change
// cases rather than resolving them silently.
func resolveInstant(w wallTime, year int, month time.Month, day, hour, minute, second int,
	loc *time.Location, res *Resolution, expr string, kind boundaryKind) time.Time {

	wr := resolveWall(year, month, day, hour, minute, second, loc)

	switch {
	case wr.Skipped:
		// Spring forward: the requested local time never happened. Section 2.4
		// says resolve to the instant the clock jumped to, and say so.
		res.note(NoteSkippedTime,
			"%s does not exist on %04d-%02d-%02d in %s — clocks jumped forward. Using %s instead.",
			clockText(hour, minute, second), year, int(month), day, loc,
			formatLocal(wr.First, loc))

	case wr.Repeated:
		// Autumn back: the local time happened twice. Section 2.4 says include
		// both occurrences and say so, so a start takes the earlier and an end
		// takes the later — the widest reading, which cannot hide records.
		chosen := wr.First
		if kind == boundaryEnd {
			chosen = wr.Second
		}
		res.note(NoteRepeatedTime,
			"%s occurs twice on %04d-%02d-%02d in %s — clocks went back. Both occurrences are included (%s and %s).",
			clockText(hour, minute, second), year, int(month), day, loc,
			formatUTC(wr.First), formatUTC(wr.Second))
		return chosen
	}

	return wr.First
}

func clockText(hour, minute, second int) string {
	if second == 0 {
		return fmt.Sprintf("%02d:%02d", hour, minute)
	}
	return fmt.Sprintf("%02d:%02d:%02d", hour, minute, second)
}

// resolveRange resolves a two-ended window.
func resolveRange(expr string, tc TimeContext, res *Resolution) (Interval, error) {
	lo, hi, ok := splitRange(expr)
	if !ok {
		return Interval{}, &TimeError{
			Expr:     expr,
			Reason:   "not a time range",
			Examples: []string{"between:14:00-15:00", "14:00-14:05:30", "on:2026-08-13"},
		}
	}

	start, err := resolveBoundary(lo, tc, res, boundaryStart)
	if err != nil {
		return Interval{}, err
	}
	end, err := resolveBoundary(hi, tc, res, boundaryEnd)
	if err != nil {
		return Interval{}, err
	}

	// A range whose end reads earlier than its start has crossed midnight:
	// 23:00-02:00 means the small hours of the following day, which is exactly
	// the window an overnight incident falls in.
	if !end.After(start) {
		end = end.AddDate(0, 0, 1)
		res.note(NoteResolvedDate, "%s crosses midnight, so it ends on %s.",
			expr, end.In(tc.location()).Format("2006-01-02"))
	}

	return Interval{Start: start, End: end}, nil
}

// datePart decides which calendar day a bare clock time belongs to.
//
// Section 2.1: a bare 14:00 resolves against the date range of the loaded data,
// not today. One day of data means that day. Several means the most recent day
// containing that time, and the chosen date is always printed. Never guess
// silently.
func datePart(w wallTime, tc TimeContext, res *Resolution, expr string) (int, time.Month, int, error) {
	if w.HasDate {
		return w.Year, w.Month, w.Day, nil
	}

	loc := tc.location()

	if tc.Newest.IsZero() {
		// No timestamped data to resolve against. Falling back to today is the
		// only option, and it has to be stated.
		now := tc.Now
		if now.IsZero() {
			now = time.Now()
		}
		y, m, d := now.In(loc).Date()
		res.note(NoteResolvedDate,
			"%s resolved to today (%04d-%02d-%02d): no records carry a timestamp to resolve against.",
			expr, y, int(m), d)
		return y, m, d, nil
	}

	oldest := tc.Oldest.In(loc)
	newest := tc.Newest.In(loc)

	// Walk back from the newest day to the oldest, taking the most recent day
	// on which this clock time falls inside the data's range.
	candidate := time.Date(newest.Year(), newest.Month(), newest.Day(),
		w.Hour, w.Minute, w.Second, 0, loc)

	for day := 0; day <= 366; day++ {
		at := candidate.AddDate(0, 0, -day)
		if at.After(newest) {
			continue
		}
		if at.Before(oldest) {
			break
		}
		y, m, d := at.Date()
		if !sameDay(at, newest) {
			res.note(NoteResolvedDate, "%s resolved to %04d-%02d-%02d.", expr, y, int(m), d)
		}
		return y, m, d, nil
	}

	// The clock time falls outside the data's range on every day it covers.
	// Use the newest day and let the empty-result explanation say the rest.
	y, m, d := newest.Date()
	res.note(NoteResolvedDate,
		"%s resolved to %04d-%02d-%02d, which is outside the range the data covers (%s to %s).",
		expr, y, int(m), d,
		oldest.Format("2006-01-02 15:04"), newest.Format("2006-01-02 15:04"))
	return y, m, d, nil
}

// TimeError is an unparseable or unresolvable time, carrying working examples.
//
// Section 7: an error message that includes a copyable correct example is worth
// more than any amount of documentation.
type TimeError struct {
	Expr     string
	Reason   string
	Examples []string
}

func (e *TimeError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "cannot understand the time %q", e.Expr)
	if e.Reason != "" {
		fmt.Fprintf(&sb, ": %s", e.Reason)
	}
	if len(e.Examples) > 0 {
		fmt.Fprintf(&sb, "\ntry: %s", strings.Join(e.Examples, "   "))
	}
	return sb.String()
}
