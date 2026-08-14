package query

import (
	"fmt"
	"time"
)

// wallResolution is the outcome of placing a local wall-clock time on the
// actual timeline.
//
// Most of the year this is a formality. Twice a year it is not, and the whole
// reason this type exists is that both awkward cases must be reported rather
// than silently resolved. Everything here uses time.LoadLocation's tz database
// through the standard library; no offset is ever computed arithmetically.
type wallResolution struct {
	// First is the earliest instant with the requested wall clock.
	First time.Time
	// Second is set when the wall clock occurs twice, which happens in the
	// hour repeated when clocks go back.
	Second   time.Time
	Repeated bool

	// Skipped is true when the wall clock never occurred, which happens in the
	// hour skipped when clocks go forward.
	Skipped bool
}

// resolveWall places a local date and time in a location, detecting the two
// clock-change cases.
//
// Go's time.Date always returns an instant, normalising a nonexistent local
// time forward and choosing one of the two candidates for a repeated one. That
// is convenient and, for this tool, dangerous: docs/FILTER-DSL.md section 2.4
// requires that neither case be resolved silently.
func resolveWall(year int, month time.Month, day, hour, minute, second int, loc *time.Location) wallResolution {
	t := time.Date(year, month, day, hour, minute, second, 0, loc)

	// If the resulting wall clock differs from what was asked for, the
	// requested time does not exist: the clock jumped over it.
	//
	// Go normalises such a time by applying the pre-transition offset, which
	// lands somewhere after the gap rather than at its edge. Section 2.4 asks
	// for the instant the clock jumped to, so the transition itself is found
	// and used — 01:30 on a spring-forward morning becomes 02:00, not 02:30.
	if t.Hour() != hour || t.Minute() != minute || t.Day() != day {
		if at, ok := forwardJump(year, month, day, loc); ok {
			return wallResolution{First: at, Skipped: true}
		}
		return wallResolution{First: t, Skipped: true}
	}

	// Look for a second instant with the same wall clock. Offsets change by an
	// hour almost everywhere and by thirty minutes on Lord Howe Island, so a
	// window either side at half-hour steps covers the real cases.
	_, offset := t.Zone()
	for _, delta := range []time.Duration{
		-2 * time.Hour, -90 * time.Minute, -time.Hour, -30 * time.Minute,
		30 * time.Minute, time.Hour, 90 * time.Minute, 2 * time.Hour,
	} {
		candidate := t.Add(delta)
		if _, candidateOffset := candidate.Zone(); candidateOffset == offset {
			continue
		}
		// A different offset only matters if the wall clock still reads the
		// same, which is exactly the repeated-hour case.
		if candidate.Year() == year && candidate.Month() == month && candidate.Day() == day &&
			candidate.Hour() == hour && candidate.Minute() == minute && candidate.Second() == second {

			first, second := t, candidate
			if candidate.Before(t) {
				first, second = candidate, t
			}
			return wallResolution{First: first, Second: second, Repeated: true}
		}
	}

	return wallResolution{First: t}
}

// forwardJump finds the instant clocks moved forward on a given local day.
//
// It is only consulted when a requested wall clock turned out not to exist, so
// the day is known to contain a gap.
func forwardJump(year int, month time.Month, day int, loc *time.Location) (time.Time, bool) {
	dayStart := time.Date(year, month, day, 0, 0, 0, 0, loc)

	for _, tr := range findTransitions(Interval{Start: dayStart, End: dayStart.AddDate(0, 0, 1)}, loc) {
		if tr.Shift > 0 {
			return tr.At, true
		}
	}
	return time.Time{}, false
}

// transition is a clock change found inside a window.
type transition struct {
	At time.Time
	// FromZone and ToZone are the abbreviations either side, e.g. BST and GMT.
	FromZone, ToZone string
	// Shift is how much the offset moved. Negative means clocks went back.
	Shift time.Duration
}

func (tr transition) String() string {
	direction := "forward"
	if tr.Shift < 0 {
		direction = "back"
	}
	return fmt.Sprintf("clocks went %s %s at %s (%s → %s)",
		direction, humanDuration(tr.Shift), tr.At.Format("2006-01-02 15:04"), tr.FromZone, tr.ToZone)
}

// humanDuration renders a duration the way a person writes it: 1h rather than
// Go's 1h0m0s, and 8h30m rather than 8h30m0s.
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	d = d.Round(time.Second)

	hours := int(d / time.Hour)
	minutes := int((d % time.Hour) / time.Minute)
	seconds := int((d % time.Minute) / time.Second)

	switch {
	case hours > 0 && minutes == 0 && seconds == 0:
		return fmt.Sprintf("%dh", hours)
	case hours > 0 && seconds == 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh%dm%ds", hours, minutes, seconds)
	case minutes > 0 && seconds == 0:
		return fmt.Sprintf("%dm", minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm%ds", minutes, seconds)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}

// findTransitions returns every offset change strictly inside an interval.
//
// A window that crosses a clock change is not the duration it appears to be,
// and in the UK the change lands at 01:00 UTC — squarely inside the kind of
// overnight window this tool gets used for. Saying nothing about it means a
// user believes they looked at five and a half hours when they looked at six
// and a half.
//
// The scan is hourly with a binary search to pin the exact instant, which costs
// nothing for the window sizes anyone queries and avoids depending on internals
// of the tz database that the standard library does not expose.
func findTransitions(i Interval, loc *time.Location) []transition {
	if i.Start.IsZero() || i.End.IsZero() || !i.Start.Before(i.End) {
		return nil
	}
	// A scan over an unbounded or absurd range is not worth doing; a year of
	// hourly steps is already far more than any real query window.
	if i.Duration() > 400*24*time.Hour {
		return nil
	}

	var out []transition

	prev := i.Start.In(loc)
	_, prevOffset := prev.Zone()

	for t := i.Start.In(loc); t.Before(i.End); {
		next := t.Add(time.Hour)
		if next.After(i.End) {
			next = i.End.In(loc)
		}

		_, offset := next.Zone()
		if offset != prevOffset {
			at := pinTransition(prev, next, loc, prevOffset)
			fromZone, _ := prev.Zone()
			toZone, _ := at.Zone()
			out = append(out, transition{
				At:       at,
				FromZone: fromZone,
				ToZone:   toZone,
				Shift:    time.Duration(offset-prevOffset) * time.Second,
			})
			prevOffset = offset
		}

		if !next.After(t) {
			break
		}
		prev, t = next, next
	}

	return out
}

// pinTransition binary-searches for the second at which the offset changed.
func pinTransition(lo, hi time.Time, loc *time.Location, loOffset int) time.Time {
	for hi.Sub(lo) > time.Second {
		mid := lo.Add(hi.Sub(lo) / 2)
		if _, offset := mid.In(loc).Zone(); offset == loOffset {
			lo = mid
		} else {
			hi = mid
		}
	}
	return hi.In(loc)
}
