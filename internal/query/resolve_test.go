package query

import (
	"strings"
	"testing"
	"time"
)

// London is the reference zone for the DST tests because docs/FILTER-DSL.md
// uses it, and because its clock change lands at 01:00 UTC — squarely inside
// the kind of overnight window this tool gets used for.
func london(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	return loc
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", s, err)
	}
	return ts
}

// resolve parses and resolves a filter in one step.
func resolve(t *testing.T, filter string, tc TimeContext) (Query, Resolution) {
	t.Helper()
	q := mustParse(t, filter)
	out, res, err := ResolveTime(q, tc)
	if err != nil {
		t.Fatalf("ResolveTime(%q): %v", filter, err)
	}
	return out, res
}

// context builds a TimeContext over a day of data on 2026-08-13 UTC.
func context(t *testing.T, loc *time.Location) TimeContext {
	t.Helper()
	return TimeContext{
		Loc:    loc,
		Oldest: mustTime(t, "2026-08-13T00:10:00Z"),
		Newest: mustTime(t, "2026-08-13T23:50:00Z"),
		Now:    mustTime(t, "2026-08-20T09:00:00Z"),
	}
}

func TestResolveBasicWindows(t *testing.T) {
	tc := context(t, time.UTC)

	tests := []struct {
		name      string
		filter    string
		wantStart string
		wantEnd   string
	}{
		{
			name:      "between is inclusive of the start and exclusive of the end",
			filter:    "between:14:00-15:00",
			wantStart: "2026-08-13T14:00:00Z",
			wantEnd:   "2026-08-13T15:00:00Z",
		},
		{
			name:      "a bare range needs no keyword",
			filter:    "14:00-15:00",
			wantStart: "2026-08-13T14:00:00Z",
			wantEnd:   "2026-08-13T15:00:00Z",
		},
		{
			name:      "seconds are allowed",
			filter:    "between:14:00-14:05:30",
			wantStart: "2026-08-13T14:00:00Z",
			wantEnd:   "2026-08-13T14:05:30Z",
		},
		{
			name:      "after alone leaves the end open",
			filter:    "after:14:00",
			wantStart: "2026-08-13T14:00:00Z",
		},
		{
			name:    "before alone leaves the start open",
			filter:  "before:15:00",
			wantEnd: "2026-08-13T15:00:00Z",
		},
		{
			name:      "since is an alias for after",
			filter:    "since:14:00",
			wantStart: "2026-08-13T14:00:00Z",
		},
		{
			name:    "until is an alias for before",
			filter:  "until:15:00",
			wantEnd: "2026-08-13T15:00:00Z",
		},
		{
			name:      "on covers the whole calendar day",
			filter:    "on:2026-08-13",
			wantStart: "2026-08-13T00:00:00Z",
			wantEnd:   "2026-08-14T00:00:00Z",
		},
		{
			name:      "full RFC3339 is taken as written",
			filter:    "after:2026-08-13T14:00:00Z",
			wantStart: "2026-08-13T14:00:00Z",
		},
		{
			// Terms of the same kind intersect rather than override.
			name:      "two afters narrow to the later one",
			filter:    "after:14:00 after:14:30",
			wantStart: "2026-08-13T14:30:00Z",
		},
		{
			name:      "after and before compose into a window",
			filter:    "after:14:00 before:15:00",
			wantStart: "2026-08-13T14:00:00Z",
			wantEnd:   "2026-08-13T15:00:00Z",
		},
		{
			name:      "on and a bare range intersect",
			filter:    "on:2026-08-13 14:11-14:14",
			wantStart: "2026-08-13T14:11:00Z",
			wantEnd:   "2026-08-13T14:14:00Z",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, res := resolve(t, tt.filter, tc)

			if tt.wantStart == "" {
				if !res.Interval.Start.IsZero() {
					t.Errorf("Start = %v, want unbounded", res.Interval.Start)
				}
			} else if got := res.Interval.Start.UTC().Format(time.RFC3339); got != tt.wantStart {
				t.Errorf("Start = %s, want %s", got, tt.wantStart)
			}

			if tt.wantEnd == "" {
				if !res.Interval.End.IsZero() {
					t.Errorf("End = %v, want unbounded", res.Interval.End)
				}
			} else if got := res.Interval.End.UTC().Format(time.RFC3339); got != tt.wantEnd {
				t.Errorf("End = %s, want %s", got, tt.wantEnd)
			}
		})
	}
}

// All four bare-time shapes must be accepted; they cost little and remove a
// whole class of "why didn't this work" reports.
func TestAcceptedClockShapes(t *testing.T) {
	tc := context(t, time.UTC)

	for _, filter := range []string{
		"after:14:00", "after:1400", "after:14:00:00", "after:2:00pm", "after:2pm",
	} {
		t.Run(filter, func(t *testing.T) {
			_, res := resolve(t, filter, tc)
			if got := res.Interval.Start.UTC().Format("15:04:05"); got != "14:00:00" {
				t.Errorf("%s resolved to %s, want 14:00:00", filter, got)
			}
		})
	}
}

// The last: trap. Relative to the newest record, not wall clock — otherwise
// last:15m on a log file from yesterday returns nothing, which is the single
// most confusing possible result.
func TestLastIsRelativeToTheNewestRecord(t *testing.T) {
	tc := context(t, time.UTC)

	_, res := resolve(t, "last:15m", tc)

	wantStart := mustTime(t, "2026-08-13T23:35:00Z")
	if !res.Interval.Start.Equal(wantStart) {
		t.Errorf("Start = %v, want %v (15m before the newest record)", res.Interval.Start, wantStart)
	}

	// The newest record must fall inside its own last: window.
	if !res.Interval.Contains(tc.Newest) {
		t.Error("the newest record is not inside last:15m")
	}

	// And the anchor must be stated, or the user cannot tell which it used.
	if !hasNote(res, NoteAnchor) {
		t.Error("no note explaining what last: measured back from")
	}
	if !strings.Contains(noteText(res, NoteAnchor), "newest record") {
		t.Errorf("anchor note does not name the anchor: %q", noteText(res, NoteAnchor))
	}
}

// --relative-to=now and --follow measure from the wall clock instead.
func TestLastRelativeToNow(t *testing.T) {
	tc := context(t, time.UTC)
	tc.RelativeToNow = true

	_, res := resolve(t, "last:15m", tc)

	wantStart := mustTime(t, "2026-08-20T08:45:00Z")
	if !res.Interval.Start.Equal(wantStart) {
		t.Errorf("Start = %v, want %v (15m before now)", res.Interval.Start, wantStart)
	}
	if !strings.Contains(noteText(res, NoteAnchor), "wall clock") {
		t.Errorf("anchor note should say wall clock: %q", noteText(res, NoteAnchor))
	}
}

func TestLastDurationUnits(t *testing.T) {
	tc := context(t, time.UTC)

	tests := map[string]time.Duration{
		"last:90s": 90 * time.Second,
		"last:15m": 15 * time.Minute,
		"last:2h":  2 * time.Hour,
		"last:3d":  72 * time.Hour,
		"last:1w":  7 * 24 * time.Hour,
	}

	for filter, want := range tests {
		t.Run(filter, func(t *testing.T) {
			_, res := resolve(t, filter, tc)
			got := tc.Newest.Sub(res.Interval.Start)
			if got != want {
				t.Errorf("window = %v, want %v", got, want)
			}
		})
	}
}

// Bare times resolve against the data's date range, not today, and the chosen
// date is reported. Never resolve silently.
func TestBareTimeResolvesAgainstTheDataAndSaysWhich(t *testing.T) {
	// Data spanning three days. A bare 14:00 must land on the most recent day
	// containing it.
	tc := TimeContext{
		Loc:    time.UTC,
		Oldest: mustTime(t, "2026-08-11T08:00:00Z"),
		Newest: mustTime(t, "2026-08-13T23:00:00Z"),
	}

	_, res := resolve(t, "after:14:00", tc)

	if got := res.Interval.Start.UTC().Format("2006-01-02"); got != "2026-08-13" {
		t.Errorf("resolved to %s, want the most recent day 2026-08-13", got)
	}
}

// When the chosen day is not the newest, the date must be printed.
func TestBareTimeReportsTheResolvedDate(t *testing.T) {
	// The newest record is at 09:00, so a bare 14:00 cannot be on that day.
	tc := TimeContext{
		Loc:    time.UTC,
		Oldest: mustTime(t, "2026-08-11T08:00:00Z"),
		Newest: mustTime(t, "2026-08-13T09:00:00Z"),
	}

	_, res := resolve(t, "after:14:00", tc)

	if !hasNote(res, NoteResolvedDate) {
		t.Fatalf("no note naming the resolved date; the user cannot tell which day was used")
	}
	if got := res.Interval.Start.UTC().Format("2006-01-02"); got != "2026-08-12" {
		t.Errorf("resolved to %s, want 2026-08-12 — the most recent day containing 14:00", got)
	}
	if !strings.Contains(noteText(res, NoteResolvedDate), "2026-08-12") {
		t.Errorf("note does not name the date: %q", noteText(res, NoteResolvedDate))
	}
}

// A range whose end reads earlier than its start has crossed midnight, which is
// exactly when overnight incidents happen.
func TestRangeSpanningMidnight(t *testing.T) {
	tc := context(t, time.UTC)

	_, res := resolve(t, "23:00-02:00", tc)

	if res.Interval.Duration() != 3*time.Hour {
		t.Errorf("duration = %v, want 3h", res.Interval.Duration())
	}
	if got := res.Interval.Start.UTC().Format("2006-01-02 15:04"); got != "2026-08-13 23:00" {
		t.Errorf("Start = %s", got)
	}
	if got := res.Interval.End.UTC().Format("2006-01-02 15:04"); got != "2026-08-14 02:00" {
		t.Errorf("End = %s, want the following day", got)
	}
	if !hasNote(res, NoteResolvedDate) {
		t.Error("crossing midnight was not reported")
	}
}

// Spring forward: 01:30 on the changeover day never happens in London. Section
// 2.4 requires resolving to the instant the clock jumped to, and saying so.
func TestDSTSpringForwardGap(t *testing.T) {
	loc := london(t)

	// Clocks go forward at 01:00 GMT on 2026-03-29, so 01:00–01:59 local does
	// not exist that day.
	tc := TimeContext{
		Loc:    loc,
		Oldest: mustTime(t, "2026-03-29T00:00:00Z"),
		Newest: mustTime(t, "2026-03-29T12:00:00Z"),
	}

	_, res := resolve(t, "after:2026-03-29T01:30:00", tc)

	if !hasNote(res, NoteSkippedTime) {
		t.Fatal("a nonexistent local time was resolved silently")
	}

	note := noteText(res, NoteSkippedTime)
	for _, want := range []string{"does not exist", "jumped forward"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q missing %q", note, want)
		}
	}

	// It must still resolve to a real instant, not fail.
	if res.Interval.Start.IsZero() {
		t.Error("no instant chosen for the skipped time")
	}
	// And that instant is the moment the clock jumped to: 01:00 GMT = 02:00 BST.
	if got := res.Interval.Start.UTC().Format("15:04"); got != "01:00" {
		t.Errorf("resolved to %s UTC, want 01:00 UTC — the instant of the jump", got)
	}
}

// Autumn back: 01:30 happens twice in London on the changeover day. Section 2.4
// requires including both occurrences and saying so, never silently picking one.
func TestDSTAutumnBackAmbiguity(t *testing.T) {
	loc := london(t)

	// Clocks go back at 02:00 BST on 2026-10-25, so 01:00–01:59 local happens
	// twice: once at BST (UTC+1) and again at GMT (UTC+0).
	tc := TimeContext{
		Loc:    loc,
		Oldest: mustTime(t, "2026-10-25T00:00:00Z"),
		Newest: mustTime(t, "2026-10-25T12:00:00Z"),
	}

	_, res := resolve(t, "between:2026-10-25T01:30-2026-10-25T01:45", tc)

	if !hasNote(res, NoteRepeatedTime) {
		t.Fatal("an ambiguous local time was resolved silently")
	}
	if !strings.Contains(noteText(res, NoteRepeatedTime), "occurs twice") {
		t.Errorf("note does not say the time is repeated: %q", noteText(res, NoteRepeatedTime))
	}

	// Both occurrences must be inside the window. The start takes the earlier
	// instant and the end the later, which is the widest reading and therefore
	// the one that cannot hide records.
	first := mustTime(t, "2026-10-25T00:30:00Z")  // 01:30 BST
	second := mustTime(t, "2026-10-25T01:30:00Z") // 01:30 GMT

	if !res.Interval.Contains(first) {
		t.Errorf("the first 01:30 (%s) is outside the window %s", first, res.Interval)
	}
	if !res.Interval.Contains(second) {
		t.Errorf("the second 01:30 (%s) is outside the window %s", second, res.Interval)
	}
}

// A window crossing a clock change is not the duration it appears to be, and
// saying nothing means the user believes a wrong number.
func TestDSTTransitionInsideWindowIsReported(t *testing.T) {
	loc := london(t)

	tc := TimeContext{
		Loc:    loc,
		Oldest: mustTime(t, "2026-10-24T00:00:00Z"),
		Newest: mustTime(t, "2026-10-26T00:00:00Z"),
	}

	// 00:00 to 07:30 local on the day the clocks go back is eight and a half
	// hours of real time, not seven and a half.
	_, res := resolve(t, "between:2026-10-25T00:00-2026-10-25T07:30", tc)

	if !hasNote(res, NoteDST) {
		t.Fatal("a clock change inside the window was not reported")
	}

	note := noteText(res, NoteDST)
	if !strings.Contains(note, "back") {
		t.Errorf("note does not say which way the clocks went: %q", note)
	}

	if got := res.Interval.Duration(); got != 8*time.Hour+30*time.Minute {
		t.Errorf("duration = %v, want 8h30m of real time", got)
	}
}

// A window with no clock change must not carry a DST note. Noise is how real
// warnings get ignored.
func TestNoDSTNoteWhenNothingChanges(t *testing.T) {
	loc := london(t)
	tc := context(t, loc)

	_, res := resolve(t, "between:14:00-15:00", tc)

	if hasNote(res, NoteDST) {
		t.Errorf("spurious DST note: %q", noteText(res, NoteDST))
	}
}

// Bare times are interpreted in the display timezone, so the same query means
// different instants in different zones. Explicit offsets override it.
func TestBareTimesUseTheDisplayTimezone(t *testing.T) {
	loc := london(t)

	utcCtx := context(t, time.UTC)
	londonCtx := context(t, loc)

	_, inUTC := resolve(t, "after:14:00", utcCtx)
	_, inLondon := resolve(t, "after:14:00", londonCtx)

	// August is BST, one hour ahead, so 14:00 London is 13:00 UTC.
	if diff := inUTC.Interval.Start.Sub(inLondon.Interval.Start); diff != time.Hour {
		t.Errorf("difference = %v, want 1h; the display timezone was not applied", diff)
	}

	// An explicit offset wins regardless of the display timezone.
	_, explicit := resolve(t, "after:2026-08-13T14:00:00Z", londonCtx)
	if got := explicit.Interval.Start.UTC().Format("15:04"); got != "14:00" {
		t.Errorf("explicit offset was overridden by the display timezone: got %s", got)
	}
}

// Negated time terms exclude a window rather than bounding one.
func TestNegatedTimeTermExcludes(t *testing.T) {
	tc := context(t, time.UTC)

	_, res := resolve(t, "on:2026-08-13 -between:14:00-15:00", tc)

	if len(res.Exclude) != 1 {
		t.Fatalf("got %d excluded windows, want 1", len(res.Exclude))
	}
	if res.Interval.Duration() != 24*time.Hour {
		t.Errorf("the positive window changed: %v", res.Interval.Duration())
	}
}

func TestTimeErrorsCarryExamples(t *testing.T) {
	tc := context(t, time.UTC)

	tests := []struct {
		filter string
		want   string
	}{
		{"last:15", "last:15m"},
		{"last:banana", "last:15m"},
		{"on:notadate", "on:2026-08-13"},
		{"after:25:99", "14:00"},
	}

	for _, tt := range tests {
		t.Run(tt.filter, func(t *testing.T) {
			q := mustParse(t, tt.filter)
			_, _, err := ResolveTime(q, tc)
			if err == nil {
				t.Fatalf("ResolveTime(%q) succeeded, want an error", tt.filter)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error lacks a copyable example %q:\n%v", tt.want, err)
			}
		})
	}
}

// A resolved window renders as absolute instants so a shared query means the
// same thing on another machine.
func TestResolvedTermRendersAbsolutely(t *testing.T) {
	loc := london(t)
	tc := context(t, loc)

	out, _ := resolve(t, "between:14:00-15:00", tc)

	rendered := out.String()
	if !strings.Contains(rendered, "2026-08-13T13:00:00Z") {
		t.Errorf("rendered %q, want absolute UTC instants", rendered)
	}

	// Re-resolving the rendered form must give back the same window, or a
	// pasted query would mean something different to its recipient.
	reparsed := mustParse(t, rendered)
	_, again, err := ResolveTime(reparsed, tc)
	if err != nil {
		t.Fatalf("re-resolving %q: %v", rendered, err)
	}

	if !again.Interval.Start.Equal(res(t, "between:14:00-15:00", tc).Start) {
		t.Errorf("re-resolved start %v differs from the original", again.Interval.Start)
	}
	if !again.Interval.End.Equal(res(t, "between:14:00-15:00", tc).End) {
		t.Errorf("re-resolved end %v differs from the original", again.Interval.End)
	}
}

// res is a shorthand returning just the resolved interval.
func res(t *testing.T, filter string, tc TimeContext) Interval {
	t.Helper()
	_, r := resolve(t, filter, tc)
	return r.Interval
}

func hasNote(res Resolution, kind NoteKind) bool {
	for _, n := range res.Notes {
		if n.Kind == kind {
			return true
		}
	}
	return false
}

func noteText(res Resolution, kind NoteKind) string {
	for _, n := range res.Notes {
		if n.Kind == kind {
			return n.Text
		}
	}
	return ""
}
