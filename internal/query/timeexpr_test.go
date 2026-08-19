package query

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"30s", 30 * time.Second},
		{"15m", 15 * time.Minute},
		{"2h", 2 * time.Hour},
		{"3d", 72 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
		{"1.5h", 90 * time.Minute},
		{"0m", 0},
		// Days are the unit people most often want here, and Go's
		// time.ParseDuration does not understand them.
		{"90d", 90 * 24 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseDuration(tt.in)
			if err != nil {
				t.Fatalf("ParseDuration(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("= %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseDurationRejects(t *testing.T) {
	for _, in := range []string{"", "15", "banana", "m", "-5m", "15x"} {
		t.Run(in, func(t *testing.T) {
			if _, err := ParseDuration(in); err == nil {
				t.Errorf("ParseDuration(%q) succeeded, want an error", in)
			}
		})
	}
}

func TestParseClockShapes(t *testing.T) {
	tests := []struct {
		in                   string
		hour, minute, second int
	}{
		{"14:00", 14, 0, 0},
		{"14:00:00", 14, 0, 0},
		{"14:05:30", 14, 5, 30},
		{"1400", 14, 0, 0},
		{"140530", 14, 5, 30},
		{"2:00pm", 14, 0, 0},
		{"2pm", 14, 0, 0},
		{"12:00am", 0, 0, 0},
		{"12:00pm", 12, 0, 0},
		{"11:30am", 11, 30, 0},
		{"0:00", 0, 0, 0},
		{"23:59:59", 23, 59, 59},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseClock(tt.in)
			if err != nil {
				t.Fatalf("parseClock(%q): %v", tt.in, err)
			}
			if got.Hour != tt.hour || got.Minute != tt.minute || got.Second != tt.second {
				t.Errorf("= %02d:%02d:%02d, want %02d:%02d:%02d",
					got.Hour, got.Minute, got.Second, tt.hour, tt.minute, tt.second)
			}
		})
	}
}

func TestParseClockRejectsOutOfRange(t *testing.T) {
	for _, in := range []string{"25:00", "14:60", "14:00:60", "banana", "", "1:2:3:4"} {
		t.Run(in, func(t *testing.T) {
			if _, err := parseClock(in); err == nil {
				t.Errorf("parseClock(%q) succeeded, want an error", in)
			}
		})
	}
}

func TestParseWallTimeShapes(t *testing.T) {
	tests := []struct {
		in       string
		wantZone bool
		wantDate bool
		wantTime bool
	}{
		{"2026-08-13T14:00:00Z", true, false, false},
		{"2026-08-13T14:00:00+01:00", true, false, false},
		{"2026-08-13T14:00:00", false, true, true},
		{"2026-08-13 14:00", false, true, true},
		{"2026-08-13", false, true, false},
		{"14:00", false, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseWallTime(tt.in)
			if err != nil {
				t.Fatalf("parseWallTime(%q): %v", tt.in, err)
			}
			if got.HasZone != tt.wantZone {
				t.Errorf("HasZone = %v, want %v", got.HasZone, tt.wantZone)
			}
			if got.HasDate != tt.wantDate {
				t.Errorf("HasDate = %v, want %v", got.HasDate, tt.wantDate)
			}
			if got.HasClock != tt.wantTime {
				t.Errorf("HasClock = %v, want %v", got.HasClock, tt.wantTime)
			}
		})
	}
}

// Hyphens separate the halves of a range but also appear inside dates, so the
// split has to be found rather than guessed.
func TestSplitRange(t *testing.T) {
	tests := []struct {
		in     string
		lo, hi string
		wantOK bool
	}{
		{in: "14:00-15:00", lo: "14:00", hi: "15:00", wantOK: true},
		{in: "14:00-14:05:30", lo: "14:00", hi: "14:05:30", wantOK: true},
		{in: "1400-1500", lo: "1400", hi: "1500", wantOK: true},
		{
			in: "2026-08-13-2026-08-14",
			lo: "2026-08-13", hi: "2026-08-14", wantOK: true,
		},
		{
			in: "2026-10-25T01:30-2026-10-25T01:45",
			lo: "2026-10-25T01:30", hi: "2026-10-25T01:45", wantOK: true,
		},
		{in: "notarange", wantOK: false},
		{in: "14:00", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			lo, hi, ok := splitRange(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (got %q / %q)", ok, tt.wantOK, lo, hi)
			}
			if !ok {
				return
			}
			if lo != tt.lo || hi != tt.hi {
				t.Errorf("split = %q / %q, want %q / %q", lo, hi, tt.lo, tt.hi)
			}
		})
	}
}

func TestHumanDuration(t *testing.T) {
	tests := map[time.Duration]string{
		time.Hour:                    "1h",
		8*time.Hour + 30*time.Minute: "8h30m",
		90 * time.Minute:             "1h30m",
		45 * time.Minute:             "45m",
		30 * time.Second:             "30s",
		90 * time.Second:             "1m30s",
		-time.Hour:                   "1h",
		2*time.Hour + 3*time.Second:  "2h0m3s",
	}

	for d, want := range tests {
		t.Run(want, func(t *testing.T) {
			if got := humanDuration(d); got != want {
				t.Errorf("humanDuration(%v) = %q, want %q", d, got, want)
			}
		})
	}
}

func TestIntervalIntersect(t *testing.T) {
	at := func(h int) time.Time {
		return time.Date(2026, 8, 13, h, 0, 0, 0, time.UTC)
	}

	tests := []struct {
		name       string
		a, b       Interval
		start, end time.Time
	}{
		{
			name:  "later start wins",
			a:     Interval{Start: at(14)},
			b:     Interval{Start: at(15)},
			start: at(15),
		},
		{
			name: "earlier end wins",
			a:    Interval{End: at(15)},
			b:    Interval{End: at(14)},
			end:  at(14),
		},
		{
			name:  "open sides compose into a window",
			a:     Interval{Start: at(14)},
			b:     Interval{End: at(15)},
			start: at(14), end: at(15),
		},
		{
			name:  "unbounded intersected with a window is the window",
			a:     Interval{},
			b:     Interval{Start: at(14), End: at(15)},
			start: at(14), end: at(15),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.a.Intersect(tt.b)
			if !got.Start.Equal(tt.start) {
				t.Errorf("Start = %v, want %v", got.Start, tt.start)
			}
			if !got.End.Equal(tt.end) {
				t.Errorf("End = %v, want %v", got.End, tt.end)
			}
		})
	}
}

// Half-open: the start is inside, the end is not. That is what makes adjacent
// windows tile without double-counting a record.
func TestIntervalIsHalfOpen(t *testing.T) {
	start := time.Date(2026, 8, 13, 14, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 13, 15, 0, 0, 0, time.UTC)
	i := Interval{Start: start, End: end}

	if !i.Contains(start) {
		t.Error("the start instant should be inside")
	}
	if i.Contains(end) {
		t.Error("the end instant should be outside")
	}
	if !i.Contains(end.Add(-time.Nanosecond)) {
		t.Error("the instant just before the end should be inside")
	}
}

// A duration too large for an int64 used to wrap, usually to a negative value,
// so last:99999999999999999999d produced a window that ended before it began —
// matching nothing and explaining nothing. Found by FuzzParseDuration.
func TestParseDurationRefusesOverflow(t *testing.T) {
	for _, in := range []string{
		"99999999999999999999d",
		"1e18h",
		"1e308d",
		// NaN is not less than zero and not greater than the maximum, so it
		// passed both guards and became the most negative int64 there is.
		"nans",
		"NANw",
		"infd",
	} {
		got, err := ParseDuration(in)
		if err == nil {
			t.Errorf("ParseDuration(%q) = %s, want an error", in, got)
			continue
		}
		if got != 0 {
			t.Errorf("ParseDuration(%q) returned %s alongside its error", in, got)
		}
	}
}

// Everything inside the representable range still works, including the largest
// unit at a realistic size.
func TestParseDurationAcceptsLargeButUsableWindows(t *testing.T) {
	for _, in := range []string{"52w", "3650d", "100000h"} {
		got, err := ParseDuration(in)
		if err != nil {
			t.Errorf("ParseDuration(%q): %v", in, err)
			continue
		}
		if got <= 0 {
			t.Errorf("ParseDuration(%q) = %s, want a positive duration", in, got)
		}
	}
}
