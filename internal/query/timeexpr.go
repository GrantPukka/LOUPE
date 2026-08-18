package query

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// wallTime is a time expression that has been parsed but not yet placed on the
// calendar.
//
// A bare clock time like 14:00 has no date until it is resolved against the
// data's own date range, and no instant until a timezone is applied. Keeping
// those two steps apart is what makes the resolution reportable: the user is
// told which date was chosen and which zone was used, rather than being handed
// an instant and asked to trust it.
type wallTime struct {
	// Instant is set when the expression named an exact moment, either through
	// an explicit offset or a full local date and time.
	Instant time.Time
	HasZone bool

	// Date is set when the expression named a calendar day.
	Year    int
	Month   time.Month
	Day     int
	HasDate bool

	// Clock is set when the expression named a time of day.
	Hour, Minute, Second int
	HasClock             bool
	// Precision is how much of the clock was written, which decides what a
	// bare range's end means: 14:00-15:00 ends at 15:00:00, not 15:00:59.
	Precision clockPrecision
}

type clockPrecision int

const (
	precisionNone clockPrecision = iota
	precisionHour
	precisionMinute
	precisionSecond
)

// ParseDuration reads the duration forms accepted by last:.
//
// Go's time.ParseDuration does not understand days, which is the unit people
// most often want here, so the units are handled directly. Weeks are included
// because they cost two lines and someone will type them.
//
// Exported so a flag that takes a window — `--new-since 15m` — accepts exactly
// what the filter language does. A flag that quietly understood a different
// set of units from the DSL beside it would be its own small betrayal.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}

	units := map[byte]time.Duration{
		's': time.Second,
		'm': time.Minute,
		'h': time.Hour,
		'd': 24 * time.Hour,
		'w': 7 * 24 * time.Hour,
	}

	unit, ok := units[s[len(s)-1]]
	if !ok {
		return 0, fmt.Errorf("no unit on duration %q", s)
	}

	n, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil {
		return 0, fmt.Errorf("bad number in duration %q", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("negative duration %q", s)
	}

	return time.Duration(n * float64(unit)), nil
}

// parseWallTime reads one time expression.
//
// Accepted, per docs/FILTER-DSL.md section 2:
//
//	2026-08-13T14:00:00Z      full RFC3339, carries its own zone
//	2026-08-13T14:00:00       local date and time
//	2026-08-13 14:00          the same, space-separated
//	2026-08-13                a calendar day
//	14:00  14:00:00  1400     a time of day
//	2:00pm  2pm               the same, twelve-hour
//
// All four bare shapes are accepted because they cost little and remove a whole
// class of "why didn't this work" reports.
func parseWallTime(s string) (wallTime, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return wallTime{}, fmt.Errorf("empty time")
	}

	// An explicit offset wins over everything, including the display timezone.
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05Z0700",
		"2006-01-02T15:04Z07:00",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return wallTime{Instant: t, HasZone: true}, nil
		}
	}

	// A local date and time, which needs the display timezone but not the
	// data's date range.
	for _, layout := range []struct {
		text      string
		precision clockPrecision
	}{
		{"2006-01-02T15:04:05", precisionSecond},
		{"2006-01-02T15:04", precisionMinute},
		{"2006-01-02 15:04:05", precisionSecond},
		{"2006-01-02 15:04", precisionMinute},
	} {
		if t, err := time.Parse(layout.text, s); err == nil {
			return wallTime{
				Year: t.Year(), Month: t.Month(), Day: t.Day(), HasDate: true,
				Hour: t.Hour(), Minute: t.Minute(), Second: t.Second(), HasClock: true,
				Precision: layout.precision,
			}, nil
		}
	}

	// A bare calendar day.
	for _, layout := range []string{"2006-01-02", "2006/01/02", "20060102"} {
		if t, err := time.Parse(layout, s); err == nil {
			return wallTime{Year: t.Year(), Month: t.Month(), Day: t.Day(), HasDate: true}, nil
		}
	}

	return parseClock(s)
}

// parseClock reads a time of day in the four accepted shapes.
func parseClock(s string) (wallTime, error) {
	lower := strings.ToLower(strings.TrimSpace(s))

	// Twelve-hour forms: 2pm, 2:00pm, 11:30 am.
	meridiem := ""
	for _, suffix := range []string{"am", "pm"} {
		if strings.HasSuffix(lower, suffix) {
			meridiem = suffix
			lower = strings.TrimSpace(strings.TrimSuffix(lower, suffix))
			break
		}
	}

	var hour, minute, second int
	precision := precisionHour

	switch {
	case strings.Contains(lower, ":"):
		parts := strings.Split(lower, ":")
		if len(parts) > 3 {
			return wallTime{}, fmt.Errorf("too many parts in time %q", s)
		}

		var err error
		if hour, err = atoiRange(parts[0], 0, 23); err != nil {
			return wallTime{}, fmt.Errorf("bad hour in %q", s)
		}
		if len(parts) > 1 {
			if minute, err = atoiRange(parts[1], 0, 59); err != nil {
				return wallTime{}, fmt.Errorf("bad minute in %q", s)
			}
			precision = precisionMinute
		}
		if len(parts) > 2 {
			if second, err = atoiRange(parts[2], 0, 59); err != nil {
				return wallTime{}, fmt.Errorf("bad second in %q", s)
			}
			precision = precisionSecond
		}

	case len(lower) == 4 && allDigits(lower):
		// 1400
		var err error
		if hour, err = atoiRange(lower[:2], 0, 23); err != nil {
			return wallTime{}, fmt.Errorf("bad hour in %q", s)
		}
		if minute, err = atoiRange(lower[2:], 0, 59); err != nil {
			return wallTime{}, fmt.Errorf("bad minute in %q", s)
		}
		precision = precisionMinute

	case len(lower) == 6 && allDigits(lower):
		// 140530
		var err error
		if hour, err = atoiRange(lower[:2], 0, 23); err != nil {
			return wallTime{}, fmt.Errorf("bad hour in %q", s)
		}
		if minute, err = atoiRange(lower[2:4], 0, 59); err != nil {
			return wallTime{}, fmt.Errorf("bad minute in %q", s)
		}
		if second, err = atoiRange(lower[4:], 0, 59); err != nil {
			return wallTime{}, fmt.Errorf("bad second in %q", s)
		}
		precision = precisionSecond

	case allDigits(lower) && meridiem != "":
		// 2pm
		var err error
		if hour, err = atoiRange(lower, 0, 23); err != nil {
			return wallTime{}, fmt.Errorf("bad hour in %q", s)
		}

	default:
		return wallTime{}, fmt.Errorf("unrecognised time %q", s)
	}

	switch meridiem {
	case "pm":
		if hour < 12 {
			hour += 12
		}
	case "am":
		if hour == 12 {
			hour = 0
		}
	}

	return wallTime{
		Hour: hour, Minute: minute, Second: second,
		HasClock: true, Precision: precision,
	}, nil
}

func atoiRange(s string, lo, hi int) (int, error) {
	if s == "" || !allDigits(s) {
		return 0, fmt.Errorf("not a number: %q", s)
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("%d out of range %d-%d", n, lo, hi)
	}
	return n, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// splitRange splits a range expression at its separating hyphen.
//
// Hyphens are ambiguous here: they separate the halves of 14:00-15:00 but also
// appear inside 2026-08-13. Rather than guessing by shape, every hyphen is
// tried from the left and the first split where both halves parse wins. That
// handles 2026-08-13-2026-08-14 correctly, where the separator is the fourth
// hyphen.
func splitRange(expr string) (lo, hi string, ok bool) {
	for i := 1; i < len(expr)-1; i++ {
		if expr[i] != '-' {
			continue
		}
		left, right := expr[:i], expr[i+1:]

		// An RFC3339 offset such as +01:00 is written with a hyphen when
		// negative, so a split immediately before a digit-colon-digit offset
		// would cut a timestamp in half. Requiring both halves to parse
		// catches that.
		if _, err := parseWallTime(left); err != nil {
			continue
		}
		if _, err := parseWallTime(right); err != nil {
			continue
		}
		return left, right, true
	}
	return "", "", false
}
