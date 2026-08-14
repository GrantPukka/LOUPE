package query

import (
	"fmt"
	"time"
)

// Interval is a half-open time range [Start, End).
//
// Half-open is what makes adjacent windows tile without overlapping, and it is
// what docs/FILTER-DSL.md specifies: between:14:00-15:00 includes 14:00:00.000
// and excludes 15:00:00.000.
//
// A zero Start or End means unbounded on that side.
type Interval struct {
	Start, End time.Time
}

// Unbounded reports whether the interval constrains nothing.
func (i Interval) Unbounded() bool { return i.Start.IsZero() && i.End.IsZero() }

// Empty reports whether the interval can contain no instant at all.
func (i Interval) Empty() bool {
	return !i.Start.IsZero() && !i.End.IsZero() && !i.Start.Before(i.End)
}

// Contains reports whether t falls inside the half-open range.
func (i Interval) Contains(t time.Time) bool {
	if !i.Start.IsZero() && t.Before(i.Start) {
		return false
	}
	if !i.End.IsZero() && !t.Before(i.End) {
		return false
	}
	return true
}

// Intersect narrows this interval by another.
//
// Terms of the same kind intersect rather than override, so
// `after:14:00 after:14:30` means 14:30 onward and `after:14:00 before:15:00`
// is the same window as `between:14:00-15:00`.
func (i Interval) Intersect(o Interval) Interval {
	out := i

	if !o.Start.IsZero() && (out.Start.IsZero() || o.Start.After(out.Start)) {
		out.Start = o.Start
	}
	if !o.End.IsZero() && (out.End.IsZero() || o.End.Before(out.End)) {
		out.End = o.End
	}

	return out
}

// Duration is the length of the interval, or zero when either side is
// unbounded.
//
// Note that this is real elapsed time, not the difference between the wall
// clocks. A window crossing a clock change is not the duration it appears to
// be, which is the whole point of the DST note.
func (i Interval) Duration() time.Duration {
	if i.Start.IsZero() || i.End.IsZero() {
		return 0
	}
	return i.End.Sub(i.Start)
}

// String renders the interval as a DSL term that re-parses to the same instants.
//
// RFC3339 is used rather than the display timezone because this is what the
// UI's timeline drag writes into the filter box, and a shared or pasted query
// has to mean the same thing on somebody else's machine.
func (i Interval) String() string {
	switch {
	case i.Unbounded():
		return ""
	case i.Start.IsZero():
		return "before:" + i.End.UTC().Format(time.RFC3339)
	case i.End.IsZero():
		return "after:" + i.Start.UTC().Format(time.RFC3339)
	default:
		return "between:" + i.Start.UTC().Format(time.RFC3339) + "-" + i.End.UTC().Format(time.RFC3339)
	}
}

// Describe renders the interval in both the display timezone and UTC.
//
// docs/FILTER-DSL.md section 2.3 calls this the feature, not a nicety: someone
// working an incident at four in the morning should never have to do offset
// arithmetic, and the UTC line is what they paste into the ticket.
func (i Interval) Describe(loc *time.Location) string {
	if i.Unbounded() {
		return "all time"
	}

	switch {
	case i.Start.IsZero():
		return fmt.Sprintf("up to %s = %s",
			formatLocal(i.End, loc), formatUTC(i.End))
	case i.End.IsZero():
		return fmt.Sprintf("from %s = %s",
			formatLocal(i.Start, loc), formatUTC(i.Start))
	}

	start := i.Start.In(loc)
	end := i.End.In(loc)
	startZone, _ := start.Zone()
	endZone, _ := end.Zone()

	// Same calendar day and same zone abbreviation is the common case, and
	// repeating the date twice makes it harder to read, not easier.
	if sameDay(start, end) && startZone == endZone {
		return fmt.Sprintf("%s–%s %s  =  %s–%s UTC  ·  %s",
			start.Format("15:04:05"), end.Format("15:04:05"), startZone,
			i.Start.UTC().Format("15:04:05"), i.End.UTC().Format("15:04:05"),
			start.Format("Mon 2006-01-02"))
	}

	return fmt.Sprintf("%s  –  %s  =  %s – %s",
		formatLocal(i.Start, loc), formatLocal(i.End, loc),
		formatUTC(i.Start), formatUTC(i.End))
}

func formatLocal(t time.Time, loc *time.Location) string {
	local := t.In(loc)
	zone, _ := local.Zone()
	return local.Format("2006-01-02 15:04:05") + " " + zone
}

func formatUTC(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05") + " UTC"
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// apparentDuration is how long a window looks on a wall clock, ignoring any
// offset change inside it.
//
// It exists only to be contrasted with the real duration in the DST note: a
// window reading 00:00 to 07:30 looks like seven and a half hours, and saying
// so alongside the true figure is what makes the discrepancy land.
func apparentDuration(i Interval, loc *time.Location) time.Duration {
	if i.Start.IsZero() || i.End.IsZero() {
		return 0
	}

	start := i.Start.In(loc)
	end := i.End.In(loc)

	_, startOffset := start.Zone()
	_, endOffset := end.Zone()

	return i.End.Sub(i.Start) + time.Duration(endOffset-startOffset)*time.Second
}
