package session

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// diffFixture is a healthy window followed by an incident window: a template
// that stops, one that appears, one that goes up, one that is unchanged, and a
// record with no timestamp that belongs to neither.
func diffFixture(t *testing.T) *Session {
	t.Helper()

	var lines []string
	at := func(minute, second int) string {
		return time.Date(2026, 8, 13, 14, minute, second, 0, time.UTC).Format(time.RFC3339)
	}
	add := func(minute, second int, level, msg, extra string) {
		line := `{"ts":"` + at(minute, second) + `","level":"` + level + `","msg":"` + msg + `"`
		if extra != "" {
			line += "," + extra
		}
		lines = append(lines, line+"}")
	}

	// 14:00–14:10, the healthy window. The variable part of each message is a
	// number, so all of one shape collapses to one template — which is what
	// makes the counts below the counts the comparison should report.
	for i := 0; i < 20; i++ {
		add(i/2, (i%2)*30, "info", fmt.Sprintf("cache hit for user %d", 1000+i), "")
	}
	for i := 0; i < 4; i++ {
		add(i, 15, "info", "request completed", `"status":200`)
	}
	for i := 0; i < 2; i++ {
		add(i*4, 45, "warn", "slow query took 12 ms", "")
	}
	// 26 records in the healthy window.

	// 14:10–14:20, the incident: cache hits stop, timeouts start carrying a
	// region nothing carried before, requests keep coming at five times the
	// rate and half of them now fail, slow queries are unchanged.
	for i := 0; i < 30; i++ {
		add(10+i/3, (i%3)*20, "error", fmt.Sprintf("upstream timeout after %d ms", 500+i),
			`"region":"eu-west-1"`)
	}
	for i := 0; i < 18; i++ {
		status := 200
		if i%2 == 0 {
			status = 503
		}
		add(10+i/2, (i%2)*30, "info", "request completed", fmt.Sprintf(`"status":%d`, status))
	}
	// Twice as many slow queries in a window with twice as many records: the
	// share is identical, so it is the control that must *not* be reported.
	for i := 0; i < 4; i++ {
		add(10+i*2, 45, "warn", "slow query took 12 ms", "")
	}
	// 52 records in the incident window, exactly twice the healthy one.

	lines = append(lines, `{"level":"error","msg":"no timestamp on this one"}`)

	return openFixture(t, lines...)
}

// kindOf is the tally for one kind of comparison.
func kindOf(t *testing.T, set DiffSet, kind DiffKind) DiffCount {
	t.Helper()

	for _, c := range set.Counts {
		if c.Kind == kind {
			return c
		}
	}
	t.Fatalf("no %s count in %+v", kind, set.Counts)
	return DiffCount{}
}

// labelled finds the difference with this exact label.
func labelled(t *testing.T, set DiffSet, label string) DiffItem {
	t.Helper()

	for _, it := range set.Items {
		if it.Label == label {
			return it
		}
	}
	t.Fatalf("no difference labelled %q in %d item(s)", label, len(set.Items))
	return DiffItem{}
}

// absent reports whether nothing in the set carries this label.
func absent(set DiffSet, label string) bool {
	for _, it := range set.Items {
		if it.Label == label {
			return false
		}
	}
	return true
}

func diffOf(t *testing.T, sess *Session, q DiffQuery) DiffSet {
	t.Helper()

	if q.Before == "" {
		q.Before = "between:14:00-14:10"
	}
	if q.After == "" {
		q.After = "between:14:10-14:20"
	}
	if q.Limit == 0 {
		q.Limit = -1
	}

	set, err := sess.Diff(context.Background(), q)
	if err != nil {
		t.Fatalf("Diff(%+v): %v", q, err)
	}
	return set
}

// item finds the difference whose detail contains needle.
func item(t *testing.T, set DiffSet, needle string) DiffItem {
	t.Helper()

	for _, it := range set.Items {
		if strings.Contains(it.Detail, needle) {
			return it
		}
	}
	t.Fatalf("no difference matching %q in %d item(s): %+v", needle, len(set.Items), set.Items)
	return DiffItem{}
}

// The headline: what appeared, what vanished, and what changed rate.
func TestDiffClassifiesChanges(t *testing.T) {
	set := diffOf(t, diffFixture(t), DiffQuery{})

	tests := []struct {
		needle string
		change DiffChange
		before int64
		after  int64
	}{
		{"upstream timeout", DiffAppeared, 0, 30},
		{"cache hit", DiffVanished, 20, 0},
		{"request completed", DiffShifted, 4, 18},
	}

	for _, tc := range tests {
		got := item(t, set, tc.needle)
		if got.Change != tc.change {
			t.Errorf("%s: change = %s, want %s", tc.needle, got.Change, tc.change)
		}
		if got.Before != tc.before || got.After != tc.after {
			t.Errorf("%s: %d → %d, want %d → %d", tc.needle, got.Before, got.After, tc.before, tc.after)
		}
	}
}

// A template occurring at the same rate in both windows is not a difference,
// and listing it would bury the ones that are.
func TestDiffOmitsWhatDidNotChange(t *testing.T) {
	set := diffOf(t, diffFixture(t), DiffQuery{})

	for _, it := range set.Items {
		if strings.Contains(it.Detail, "slow query") {
			t.Errorf("an unchanged template was reported as a difference: %+v", it)
		}
	}

	// It was still looked at, which is what the two counts in the footer are
	// for: "3 of 4 differ" says something a bare "3" does not.
	templates := kindOf(t, set, DiffPattern)
	if templates.Compared != 4 {
		t.Errorf("compared %d templates, want 4", templates.Compared)
	}
	if templates.Differed != 3 {
		t.Errorf("%d templates differed, want 3", templates.Differed)
	}
}

// The checklist item: rank by most surprising, not raw delta.
func TestDiffRanksBySurpriseNotDelta(t *testing.T) {
	set := diffOf(t, diffFixture(t), DiffQuery{})

	if len(set.Items) < 2 {
		t.Fatalf("got %d item(s), want at least 2", len(set.Items))
	}

	previous := math.Inf(1)
	for i, it := range set.Items {
		if it.Surprise > previous {
			t.Errorf("item %d (%v) ranks after a less surprising one (%v)", i, it.Surprise, previous)
		}
		previous = it.Surprise
	}

	// The appearance out of nothing outranks the smaller shift, even though
	// both moved by a similar count.
	timeout := item(t, set, "upstream timeout")
	requests := item(t, set, "request completed")
	if timeout.Surprise <= requests.Surprise {
		t.Errorf("0 → 30 (%v) did not outrank 4 → 20 (%v)", timeout.Surprise, requests.Surprise)
	}
}

// The worked example from the roadmap: a field that doubled from 2 to 4 is
// noise next to one that went 0 → 300.
func TestSurpriseSeparatesNoiseFromSignal(t *testing.T) {
	const records = 10000

	noise := surprise(2, 4, records, records)
	signal := surprise(0, 300, records, records)

	if noise >= signal {
		t.Errorf("2 → 4 scored %v, which is not below 0 → 300 at %v", noise, signal)
	}
	if noise > 1 {
		t.Errorf("2 → 4 scored %v, which is high enough to crowd out real findings", noise)
	}

	// A large drop is the most informative thing either window can hold: a
	// service that stopped saying what it always said.
	if drop := surprise(9800, 480, records, records); drop <= signal {
		t.Errorf("9,800 → 480 scored %v, below 0 → 300 at %v", drop, signal)
	}

	// No change at all is not a difference.
	if flat := surprise(100, 100, records, records); flat != 0 {
		t.Errorf("100 → 100 scored %v, want 0", flat)
	}
}

// The ranking is over shares, not counts, so an item that merely moved with the
// volume is not a finding — otherwise a sixtyfold traffic rise fills the top of
// the list with one fact restated once per field.
func TestSurpriseIsAboutShareNotVolume(t *testing.T) {
	// Ten per cent of both windows, in a window that grew tenfold.
	if scaled := surprise(10, 100, 100, 1000); scaled != 0 {
		t.Errorf("10%% of both windows scored %v, want 0 — it changed only with the volume", scaled)
	}

	// The same count in a window that grew tenfold is a ninefold drop in share,
	// and that is a finding.
	held := surprise(10, 10, 100, 1000)
	if held <= 0 {
		t.Errorf("10 of 100 → 10 of 1,000 scored %v, but its share collapsed", held)
	}

	// And a share that rose scores too.
	rose := surprise(10, 500, 100, 1000)
	if rose <= 0 {
		t.Errorf("10%% → 50%% scored %v, want a positive score", rose)
	}
}

// With one window empty there is no share to compare against, so nothing is
// scored and the report says so in words rather than listing everything at zero.
func TestSurpriseNeedsBothWindows(t *testing.T) {
	if got := surprise(0, 300, 0, 1000); got != 0 {
		t.Errorf("an empty before window scored %v, want 0", got)
	}
	if got := surprise(300, 0, 1000, 0); got != 0 {
		t.Errorf("an empty after window scored %v, want 0", got)
	}
}

func TestDiffReportsUnequalWindows(t *testing.T) {
	sess := diffFixture(t)

	equal := diffOf(t, sess, DiffQuery{})
	if equal.Rates {
		t.Error("two ten-minute windows were reported as unequal")
	}

	unequal := diffOf(t, sess, DiffQuery{After: "between:14:10-14:15"})
	if !unequal.Rates {
		t.Error("a ten-minute and a five-minute window were not reported as unequal")
	}

	// The rates are per second, and they say what the counts cannot: five
	// minutes of the incident is a higher rate than ten minutes of it.
	timeout := item(t, unequal, "upstream timeout")
	if timeout.AfterRate <= 0 {
		t.Errorf("after rate = %v, want a positive rate", timeout.AfterRate)
	}
	if want := float64(timeout.After) / (5 * 60); math.Abs(timeout.AfterRate-want) > 1e-9 {
		t.Errorf("after rate = %v, want %v per second", timeout.AfterRate, want)
	}
}

// Each side's numbers must be exactly what the tool would report for that
// window on its own, or the comparison disagrees with its own listings.
func TestDiffWindowCountsMatchTheFilter(t *testing.T) {
	sess := diffFixture(t)
	set := diffOf(t, sess, DiffQuery{})

	for _, side := range []DiffWindow{set.Before, set.After} {
		want, err := sess.Count(context.Background(), plan(t, sess, side.Expr))
		if err != nil {
			t.Fatalf("Count(%q): %v", side.Expr, err)
		}
		if side.Records != want {
			t.Errorf("window %q counted %d, but the same filter matches %d",
				side.Expr, side.Records, want)
		}
	}
}

// A filter given alongside narrows both windows, exactly as writing the term
// into each of them would.
func TestDiffFilterAppliesToBothWindows(t *testing.T) {
	set := diffOf(t, diffFixture(t), DiffQuery{Filter: "level:error"})

	if set.Before.Records != 0 {
		t.Errorf("before matched %d error records, want 0", set.Before.Records)
	}
	if set.After.Records != 30 {
		t.Errorf("after matched %d error records, want 30", set.After.Records)
	}
	for _, it := range set.Items {
		if strings.Contains(it.Detail, "cache hit") {
			t.Errorf("an info-level template survived level:error: %+v", it)
		}
	}
}

// A record with no timestamp is in neither window. It is not lost, but a
// comparison of two time windows has nowhere to put it, so it is stated.
func TestDiffReportsRecordsWithNoTimestamp(t *testing.T) {
	set := diffOf(t, diffFixture(t), DiffQuery{})

	if set.NoTimestamp != 1 {
		t.Errorf("no-timestamp count = %d, want 1", set.NoTimestamp)
	}
}

// Overlapping windows are not refused — each side still reports what that
// window alone would — but the overlap is stated, or a comparison of a window
// with itself looks like a comparison of two things.
func TestDiffReportsOverlappingWindows(t *testing.T) {
	sess := diffFixture(t)

	apart := diffOf(t, sess, DiffQuery{})
	if !apart.Overlap.Empty() && !apart.Overlap.Unbounded() {
		t.Errorf("adjacent windows reported an overlap: %+v", apart.Overlap)
	}

	together := diffOf(t, sess, DiffQuery{
		Before: "between:14:00-14:15",
		After:  "between:14:05-14:20",
	})
	if together.Overlap.Empty() {
		t.Error("overlapping windows reported no overlap")
	}
}

// A window is a time expression. Anything else is a mistake worth naming,
// because a comparison of two identical unbounded windows would find nothing
// and look like an answer.
func TestDiffRequiresTimeWindows(t *testing.T) {
	sess := diffFixture(t)

	_, err := sess.Diff(context.Background(), DiffQuery{Before: "level:error", After: "last:5m"})
	if err == nil {
		t.Fatal("a non-time window was accepted")
	}
	if !strings.Contains(err.Error(), "does not name a time window") {
		t.Errorf("error does not say what is wrong:\n%v", err)
	}

	_, err = sess.Diff(context.Background(), DiffQuery{Before: "last:5m"})
	if err == nil {
		t.Fatal("a missing window was accepted")
	}
	if !strings.Contains(err.Error(), "two windows") {
		t.Errorf("error does not say two windows are needed:\n%v", err)
	}
}

// An open-ended window has no length, and a rate needs one. Bounding it by the
// data's own range is the only bound that means anything, and it is disclosed.
func TestDiffClampsAnOpenWindow(t *testing.T) {
	set := diffOf(t, diffFixture(t), DiffQuery{
		Before: "before:14:10",
		After:  "after:14:10",
	})

	if set.Before.Duration() <= 0 {
		t.Error("an open-ended window was left with no length")
	}

	found := false
	for _, note := range set.Before.Notes {
		if strings.Contains(note, "open at its") {
			found = true
		}
	}
	if !found {
		t.Errorf("the clamp was not disclosed: %v", set.Before.Notes)
	}
}

// A limit cuts the list, and what it cut is counted.
func TestDiffTruncationStatesWhatItCut(t *testing.T) {
	sess := diffFixture(t)

	set, err := sess.Diff(context.Background(), DiffQuery{
		Before: "between:14:00-14:10",
		After:  "between:14:10-14:20",
		Limit:  1,
	})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	if len(set.Items) != 1 {
		t.Fatalf("got %d item(s), want 1", len(set.Items))
	}
	if !set.Truncated {
		t.Error("a cut list did not declare itself truncated")
	}
	// Differed is the real number, whatever the limit showed, and Hidden is
	// the rest of it.
	if set.Hidden != set.Differed-1 {
		t.Errorf("hidden = %d, but %d differed and 1 was shown", set.Hidden, set.Differed)
	}
	if set.Differed < 3 {
		t.Errorf("differed = %d, want at least the three templates", set.Differed)
	}
}

// The ranking is deterministic, so one run can be compared against another.
func TestDiffIsDeterministic(t *testing.T) {
	sess := diffFixture(t)

	first := diffOf(t, sess, DiffQuery{})
	second := diffOf(t, sess, DiffQuery{})

	if len(first.Items) != len(second.Items) {
		t.Fatalf("%d items then %d", len(first.Items), len(second.Items))
	}
	for i := range first.Items {
		if first.Items[i].Key != second.Items[i].Key {
			t.Errorf("item %d was %s then %s", i, first.Items[i].Key, second.Items[i].Key)
		}
	}
}

// Fields and values are compared alongside templates, and ranked on the same
// scale, so the head of the list is the answer whatever kind it turns out to be.
func TestDiffComparesFieldsAndValues(t *testing.T) {
	set := diffOf(t, diffFixture(t), DiffQuery{})

	// A field only the incident window's records carry.
	region := labelled(t, set, "field region")
	if region.Kind != DiffField || region.Change != DiffAppeared {
		t.Errorf("field region = %+v, want an appeared field", region)
	}
	if region.Before != 0 || region.After != 30 {
		t.Errorf("field region: %d → %d, want 0 → 30", region.Before, region.After)
	}

	// A field whose share of the records grew.
	status := labelled(t, set, "field status")
	if status.Before != 4 || status.After != 18 {
		t.Errorf("field status: %d → %d, want 4 → 18", status.Before, status.After)
	}

	// A value that did not exist before.
	failed := labelled(t, set, "status=503")
	if failed.Kind != DiffValue || failed.Change != DiffAppeared || failed.After != 9 {
		t.Errorf("status=503 = %+v, want an appeared value with 9 records", failed)
	}

	// And a value whose share collapsed.
	info := labelled(t, set, "level=info")
	if info.Before != 24 || info.After != 18 {
		t.Errorf("level=info: %d → %d, want 24 → 18", info.Before, info.After)
	}
}

// A field every record carries makes up the same share of both windows, so it
// falls out under the one rule rather than needing a case of its own — which is
// what keeps "field level ×61, field path ×61, field status ×61" off the top of
// the list when traffic rises.
func TestDiffSuppressesFieldsEveryRecordCarries(t *testing.T) {
	set := diffOf(t, diffFixture(t), DiffQuery{})

	for _, name := range []string{"level", "source", "format", "parsed", "file"} {
		if !absent(set, "field "+name) {
			t.Errorf("field %s was reported, but every record in both windows carries it", name)
		}
	}

	// Its values are still compared — that is where the finding is.
	labelled(t, set, "level=error")
}

// A field with one value says nothing its own presence does not, so only the
// field row survives.
func TestDiffSuppressesTheOnlyValueOfAField(t *testing.T) {
	set := diffOf(t, diffFixture(t), DiffQuery{})

	if !absent(set, "region=eu-west-1") {
		t.Error("the only value of region was reported alongside region itself")
	}
	labelled(t, set, "field region")
}

// An item whose share is unchanged is not a difference, however much its count
// moved with the volume.
func TestDiffSuppressesWhatOnlyMovedWithVolume(t *testing.T) {
	set := diffOf(t, diffFixture(t), DiffQuery{})

	// Two slow queries in 26 records, four in 52: the same share.
	for _, it := range set.Items {
		if strings.Contains(it.Detail, "slow query") || it.Label == "level=warn" {
			t.Errorf("an item that only tracked the volume was reported: %+v", it)
		}
	}
}

// A field whose values are nearly unique per record is an identifier, not a
// category. Its values are not compared one by one, and the omission is stated.
func TestDiffSkipsHighCardinalityFields(t *testing.T) {
	var lines []string
	at := func(minute, second int) string {
		return time.Date(2026, 8, 13, 14, minute, second, 0, time.UTC).Format(time.RFC3339)
	}
	for i := 0; i < MaxDiffValues+100; i++ {
		window, offset := 0, i
		if i >= (MaxDiffValues+100)/2 {
			window, offset = 10, i-(MaxDiffValues+100)/2
		}
		lines = append(lines, fmt.Sprintf(
			`{"ts":"%s","level":"info","msg":"served","trace_id":"t%d","kind":"a"}`,
			at(window+offset/60, offset%60), i))
	}

	set := diffOf(t, openFixture(t, lines...), DiffQuery{})

	found := false
	for _, skipped := range set.Skipped {
		if skipped.Field == "trace_id" {
			found = true
			if skipped.Distinct <= MaxDiffValues {
				t.Errorf("trace_id was skipped with only %d values", skipped.Distinct)
			}
		}
	}
	if !found {
		t.Errorf("trace_id was not reported as skipped: %+v", set.Skipped)
	}

	// Its presence was still compared, and no value of it was listed.
	for _, it := range set.Items {
		if it.Kind == DiffValue && it.Key == "trace_id" {
			t.Errorf("a trace_id value was compared anyway: %+v", it)
		}
	}

	// A field with few enough values still is.
	if kindOf(t, set, DiffValue).Compared == 0 {
		t.Error("no field values were compared at all")
	}
}

// Comparing against an empty window has no share to compare against, so it is
// said in words rather than listed as every item scoring zero.
func TestDiffWithAnEmptyWindow(t *testing.T) {
	set := diffOf(t, diffFixture(t), DiffQuery{Before: "between:13:00-13:10"})

	if set.Before.Records != 0 {
		t.Fatalf("the before window matched %d records, want 0", set.Before.Records)
	}
	if len(set.Items) != 0 {
		t.Errorf("got %d item(s) against an empty window, want none", len(set.Items))
	}
}

// A window bounded by the newest record is one nanosecond longer than the span
// it names. That must not decide whether the table shows counts or rates: the
// difference is invisible, and the two lengths would print identically.
func TestUnequalLengthsIgnoresTheClampTail(t *testing.T) {
	tests := []struct {
		name          string
		before, after time.Duration
		want          bool
	}{
		{"identical", time.Hour, time.Hour, false},
		{"a nanosecond apart", time.Hour, time.Hour + time.Nanosecond, false},
		{"a millisecond apart", time.Hour, time.Hour + time.Millisecond, false},
		{"a second apart", time.Hour, time.Hour + time.Second, true},
		{"an hour against five minutes", time.Hour, 5 * time.Minute, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := unequalLengths(tc.before, tc.after); got != tc.want {
				t.Errorf("unequalLengths(%s, %s) = %v, want %v", tc.before, tc.after, got, tc.want)
			}
		})
	}
}

// A clamped window keeps the exact span it was given, tail and all, because
// that is what the rates are computed from. Only the label it prints is rounded.
func TestDiffClampedWindowKeepsTheExactSpan(t *testing.T) {
	set := diffOf(t, diffFixture(t), DiffQuery{
		Before: "between:14:00-14:10",
		After:  "after:14:10",
	})

	// The newest timestamped record is the last upstream timeout, at 14:19:40,
	// and an interval is half-open, so the window has to reach one nanosecond
	// past it for that record to fall inside.
	if want := 9*time.Minute + 40*time.Second + time.Nanosecond; set.After.Duration() != want {
		t.Errorf("the clamped window is %s, want exactly %s", set.After.Duration(), want)
	}

	// Nine minutes forty against ten minutes is a real difference, so the table
	// is in rates — and those rates come from the exact span above.
	if !set.Rates {
		t.Error("windows twenty seconds apart were treated as equal")
	}
	timeout := item(t, set, "upstream timeout")
	if want := float64(timeout.After) / set.After.Duration().Seconds(); timeout.AfterRate != want {
		t.Errorf("after rate = %v, want %v — the exact span, not the rounded label",
			timeout.AfterRate, want)
	}
}
