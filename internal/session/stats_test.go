package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// statsFixture has a numeric field, a field that is numeric for most records
// and text for one, a field only some records carry, and one record with no
// timestamp at all — the cases an aggregation has to be honest about.
func statsFixture(t *testing.T) *Session {
	t.Helper()

	return openFixture(t,
		`{"ts":"2026-08-13T14:00:00Z","level":"error","msg":"a","path":"/api/checkout","latency_ms":100}`,
		`{"ts":"2026-08-13T14:00:10Z","level":"error","msg":"b","path":"/api/checkout","latency_ms":300}`,
		`{"ts":"2026-08-13T14:00:20Z","level":"error","msg":"c","path":"/api/checkout","latency_ms":"slow"}`,
		`{"ts":"2026-08-13T14:01:00Z","level":"warn","msg":"d","path":"/api/cart","latency_ms":50}`,
		`{"ts":"2026-08-13T14:03:00Z","level":"info","msg":"e","path":"/healthz","latency_ms":2}`,
		`{"ts":"2026-08-13T14:03:30Z","level":"info","msg":"f","latency_ms":4}`,
		`{"level":"info","msg":"g","path":"/healthz","latency_ms":6}`,
	)
}

func statsOf(t *testing.T, sess *Session, filter string) StatsSet {
	t.Helper()

	plan, err := sess.PlanAggregate(context.Background(), filter)
	if err != nil {
		t.Fatalf("PlanAggregate(%q): %v", filter, err)
	}
	set, err := sess.Stats(context.Background(), plan, StatsQuery{})
	if err != nil {
		t.Fatalf("Stats(%q): %v", filter, err)
	}
	return set
}

// cell reads one value out of the result by column heading.
func cell(t *testing.T, set StatsSet, row int, header string) any {
	t.Helper()

	for i, name := range set.Result.Columns {
		if name == header {
			return set.Result.Rows[row][i]
		}
	}
	t.Fatalf("no column %q in %v", header, set.Result.Columns)
	return nil
}

// The headline: a count per group, largest first.
func TestStatsCountsByField(t *testing.T) {
	set := statsOf(t, statsFixture(t), "stats count() by level")

	if len(set.Result.Rows) != 3 {
		t.Fatalf("got %d row(s), want 3: %v", len(set.Result.Rows), set.Result.Rows)
	}
	if got := cell(t, set, 0, "level"); got != "error" {
		t.Errorf("first group = %v, want error", got)
	}
	if got := cell(t, set, 0, "count()"); got != int64(3) {
		t.Errorf("first count = %v, want 3", got)
	}

	previous := int64(1 << 62)
	for i := range set.Result.Rows {
		n := cell(t, set, i, "count()").(int64)
		if n > previous {
			t.Errorf("row %d (%d) sorted after a smaller count (%d)", i, n, previous)
		}
		previous = n
	}
}

// Grouping columns come first, aggregates after, and every heading is the DSL
// text that produced it.
func TestStatsColumnOrderAndHeadings(t *testing.T) {
	set := statsOf(t, statsFixture(t), "stats count(), avg(latency_ms) by path")

	want := []string{"path", "count()", "avg(latency_ms)"}
	if strings.Join(set.Result.Columns, ",") != strings.Join(want, ",") {
		t.Errorf("columns = %v, want %v", set.Result.Columns, want)
	}
}

func TestStatsAggregateValues(t *testing.T) {
	set := statsOf(t, statsFixture(t), `path:/api/checkout stats count(), sum(latency_ms), avg(latency_ms), min(latency_ms), max(latency_ms), p50(latency_ms)`)

	tests := map[string]any{
		"count()":         int64(3),
		"sum(latency_ms)": float64(400),
		"avg(latency_ms)": float64(200),
		"min(latency_ms)": float64(100),
		"max(latency_ms)": float64(300),
		"p50(latency_ms)": float64(200),
	}
	for header, want := range tests {
		if got := cell(t, set, 0, header); got != want {
			t.Errorf("%s = %v (%T), want %v", header, got, got, want)
		}
	}
}

// count(field) counts the records that carry it; count() counts them all.
func TestStatsCountOfAFieldCountsWhatCarriesIt(t *testing.T) {
	set := statsOf(t, statsFixture(t), "stats count(), count(path)")

	if got := cell(t, set, 0, "count()"); got != int64(7) {
		t.Errorf("count() = %v, want 7", got)
	}
	if got := cell(t, set, 0, "count(path)"); got != int64(6) {
		t.Errorf("count(path) = %v, want 6", got)
	}
}

// A record with no value for a grouping field belongs to no group. It cannot be
// shown, so it is counted and named instead of vanishing.
func TestStatsReportsRecordsMissingTheGroupingField(t *testing.T) {
	set := statsOf(t, statsFixture(t), "stats count() by path")

	if set.Matched != 7 {
		t.Errorf("matched = %d, want 7", set.Matched)
	}
	if set.Grouped != 6 {
		t.Errorf("grouped = %d, want 6", set.Grouped)
	}
	if len(set.Absent) != 1 || set.Absent[0].Field != "path" || set.Absent[0].Count != 1 {
		t.Errorf("absent = %+v, want one record missing path", set.Absent)
	}

	// And the counts in the table sum to what was grouped, so the arithmetic
	// the footer states actually holds.
	var total int64
	for i := range set.Result.Rows {
		total += cell(t, set, i, "count()").(int64)
	}
	if total != set.Grouped {
		t.Errorf("the rows sum to %d, but %d records were grouped", total, set.Grouped)
	}
}

// Aggregating a field that holds no numbers is an error, not a column of
// nothing. A summary that reports blanks where it should refuse teaches the
// reader that the data is empty.
func TestStatsNonNumericFieldErrors(t *testing.T) {
	sess := statsFixture(t)

	plan, err := sess.PlanAggregate(context.Background(), "stats avg(path)")
	if err != nil {
		t.Fatalf("PlanAggregate: %v", err)
	}
	_, err = sess.Stats(context.Background(), plan, StatsQuery{})

	var nonNumeric *NonNumericFieldError
	if !errors.As(err, &nonNumeric) {
		t.Fatalf("Stats error = %v, want a NonNumericFieldError", err)
	}
	if nonNumeric.Values != 6 {
		t.Errorf("counted %d values, want 6", nonNumeric.Values)
	}
	if !strings.Contains(nonNumeric.Error(), "loupe top path") {
		t.Errorf("the error does not offer the breakdown that would work:\n%v", nonNumeric)
	}
	if !strings.Contains(nonNumeric.Error(), "count(path)") {
		t.Errorf("the error does not offer the aggregate that would work:\n%v", nonNumeric)
	}
}

// Some values non-numeric is not an error, but it is never silent: an average
// over four of five values is not an average over five.
func TestStatsReportsValuesItCouldNotRead(t *testing.T) {
	set := statsOf(t, statsFixture(t), "stats avg(latency_ms)")

	found := false
	for _, note := range set.Notes {
		if strings.Contains(note, "1 of 7") && strings.Contains(note, "latency_ms") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes do not report the value that is not a number: %v", set.Notes)
	}
}

// A field nothing carries is a note rather than an error: the aggregate is
// well-formed, there is simply nothing in this window to read.
func TestStatsReportsAFieldNothingCarries(t *testing.T) {
	set := statsOf(t, statsFixture(t), "level:warn stats avg(latency_ms), max(line_no)")

	// line_no exists on every record, so this asserts on latency_ms only after
	// narrowing to a window that has one — the note fires for the empty case.
	empty := statsOf(t, statsFixture(t), "message~nothingmatchesthis stats avg(latency_ms)")

	if len(set.Notes) != 0 {
		t.Errorf("unexpected notes for a window that has values: %v", set.Notes)
	}
	if len(empty.Notes) == 0 || !strings.Contains(empty.Notes[0], "latency_ms") {
		t.Errorf("notes for an empty window = %v, want one naming latency_ms", empty.Notes)
	}
}

// A record with no timestamp falls in no bucket, which a time filter always has
// to say out loud.
func TestStatsBinReportsRecordsWithNoTimestamp(t *testing.T) {
	set := statsOf(t, statsFixture(t), "stats count() by bin(1m)")

	if set.NoTimestamp != 1 {
		t.Errorf("no-timestamp count = %d, want 1", set.NoTimestamp)
	}
	if set.Bin != time.Minute {
		t.Errorf("bin = %s, want 1m", set.Bin)
	}
}

// Buckets are anchored to local midnight in the display timezone, so bin(1h)
// lands on the hour on the user's clock rather than on the hour in UTC.
func TestStatsBinAnchorsToLocalMidnight(t *testing.T) {
	sess := statsFixture(t)

	// A half-hour zone is the case that catches an epoch-aligned bucket: 14:00
	// UTC is 19:30 in Kolkata, so an hourly bucket boundary has to fall on the
	// half hour in UTC to be on the hour locally.
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skipf("no tzdata for Asia/Kolkata: %v", err)
	}
	sess.Loc = loc

	set := statsOf(t, sess, "stats count() by bin(1h)")

	if set.Origin.IsZero() {
		t.Fatal("no bucket origin")
	}
	local := set.Origin.In(loc)
	if local.Hour() != 0 || local.Minute() != 0 || local.Second() != 0 {
		t.Errorf("origin %s is not local midnight in %s", local, loc)
	}

	bucket, ok := cell(t, set, 0, "bin(1h)").(time.Time)
	if !ok {
		t.Fatalf("bucket column is %T, want a time", cell(t, set, 0, "bin(1h)"))
	}
	if got := bucket.In(loc); got.Minute() != 0 {
		t.Errorf("bucket starts at %s, which is not on the hour in %s", got, loc)
	}
}

// A bucket with nothing in it is not a group and has no row, so the gap is
// counted rather than left to read as continuity.
func TestStatsCountsEmptyBuckets(t *testing.T) {
	set := statsOf(t, statsFixture(t), "stats count() by bin(1m)")

	// Records fall in the 14:00, 14:01 and 14:03 buckets: 14:02 is the hole.
	if len(set.Result.Rows) != 3 {
		t.Fatalf("got %d row(s), want 3: %v", len(set.Result.Rows), set.Result.Rows)
	}
	if set.EmptyBins != 1 {
		t.Errorf("empty buckets = %d, want 1", set.EmptyBins)
	}
}

// Ordering is by time when the grouping has a bin, whatever the counts say.
func TestStatsBinOrdersByTime(t *testing.T) {
	set := statsOf(t, statsFixture(t), "stats count() by bin(1m)")

	var previous time.Time
	for i := range set.Result.Rows {
		at := cell(t, set, i, "bin(1m)").(time.Time)
		if !previous.IsZero() && !at.After(previous) {
			t.Errorf("row %d at %s does not follow %s", i, at, previous)
		}
		previous = at
	}
}

// The clause narrows with the filter beside it, because it is the same query.
func TestStatsRespectsTheFilter(t *testing.T) {
	set := statsOf(t, statsFixture(t), "level:>=error stats count()")

	if got := cell(t, set, 0, "count()"); got != int64(3) {
		t.Errorf("count() = %v, want 3", got)
	}
	if set.Matched != 3 {
		t.Errorf("matched = %d, want 3", set.Matched)
	}
}

// With no groupings there is exactly one row, even when nothing matched: a
// count of zero is an answer.
func TestStatsWithoutGroupingAlwaysAnswers(t *testing.T) {
	set := statsOf(t, statsFixture(t), "message~nothingmatchesthis stats count()")

	if len(set.Result.Rows) != 1 {
		t.Fatalf("got %d row(s), want 1", len(set.Result.Rows))
	}
	if got := cell(t, set, 0, "count()"); got != int64(0) {
		t.Errorf("count() = %v, want 0", got)
	}
	if set.Matched != 0 {
		t.Errorf("matched = %d, want 0", set.Matched)
	}
}

// A limit cuts groups, and the result says how many there really were.
func TestStatsTruncationStatesTheRealCount(t *testing.T) {
	sess := statsFixture(t)

	plan, err := sess.PlanAggregate(context.Background(), "stats count() by level")
	if err != nil {
		t.Fatalf("PlanAggregate: %v", err)
	}
	set, err := sess.Stats(context.Background(), plan, StatsQuery{Limit: 1})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if !set.Result.Truncated {
		t.Error("a cut listing did not declare itself truncated")
	}
	if set.Result.Total != 3 {
		t.Errorf("total = %d, want 3", set.Result.Total)
	}
}

// Every other caller of Plan lists records, so a clause given to one of them is
// refused rather than dropped.
func TestPlanRefusesAnAggregation(t *testing.T) {
	sess := statsFixture(t)

	_, err := sess.Plan(context.Background(), "level:error stats count() by path")

	var unsupported *StatsUnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("Plan error = %v, want a StatsUnsupportedError", err)
	}
	if !strings.Contains(unsupported.Error(), "stats count() by path") {
		t.Errorf("the error does not name the clause:\n%v", unsupported)
	}

	// And PlanAggregate, the one caller that can render it, keeps it.
	plan, err := sess.PlanAggregate(context.Background(), "level:error stats count() by path")
	if err != nil {
		t.Fatalf("PlanAggregate: %v", err)
	}
	if plan.Query.Stats == nil {
		t.Error("PlanAggregate dropped the clause")
	}
}

func TestIsAggregate(t *testing.T) {
	tests := map[string]bool{
		"":                          false,
		"level:error":               false,
		"stats:5":                   false,
		`"stats"`:                   false,
		"stats count()":             true,
		"level:error stats count()": true,
		"stats count() by bin(1m)":  true,
		"stats":                     false, // does not parse, so not an aggregation
		"level:error stats mean(x)": false,
	}

	for filter, want := range tests {
		if got := IsAggregate(filter); got != want {
			t.Errorf("IsAggregate(%q) = %v, want %v", filter, got, want)
		}
	}
}

// A clock change inside the window moves the bucket boundaries off the local
// clock they were anchored to, because a bucket is a fixed width of real time.
// Never silently: docs/FILTER-DSL.md section 2.4 applies to anything that puts
// records in time.
func TestStatsBinReportsAClockChange(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("no tzdata for Europe/London: %v", err)
	}

	// British Summer Time ends at 02:00 on 2026-10-25, when the clocks go back
	// to 01:00. These two records straddle it.
	sess := openFixture(t,
		`{"ts":"2026-10-25T00:30:00Z","level":"info","msg":"before the change"}`,
		`{"ts":"2026-10-25T03:30:00Z","level":"info","msg":"after the change"}`,
	)
	sess.Loc = loc

	set := statsOf(t, sess, "stats count() by bin(1h)")

	found := false
	for _, note := range set.Notes {
		if strings.Contains(note, "clocks change") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes do not mention the clock change: %v", set.Notes)
	}

	// The anchor is still a real instant, found through the tz database rather
	// than by adding an offset.
	if local := set.Origin.In(loc); local.Hour() != 0 || local.Minute() != 0 {
		t.Errorf("origin %s is not local midnight in %s", local, loc)
	}
}

// A window crossing midnight buckets across the day boundary without a gap and
// without restarting: the anchor is one instant, not one per day.
func TestStatsBinAcrossMidnight(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("no tzdata for Europe/London: %v", err)
	}

	sess := openFixture(t,
		`{"ts":"2026-08-13T22:30:00Z","level":"info","msg":"a"}`,
		`{"ts":"2026-08-13T23:30:00Z","level":"info","msg":"b"}`,
		`{"ts":"2026-08-14T00:30:00Z","level":"info","msg":"c"}`,
	)
	sess.Loc = loc

	set := statsOf(t, sess, "stats count() by bin(1h)")

	if len(set.Result.Rows) != 3 {
		t.Fatalf("got %d row(s), want 3: %v", len(set.Result.Rows), set.Result.Rows)
	}
	for i := range set.Result.Rows {
		at := cell(t, set, i, "bin(1h)").(time.Time).In(loc)
		if at.Minute() != 0 || at.Second() != 0 {
			t.Errorf("bucket %d starts at %s, which is not on the hour locally", i, at)
		}
	}
	if set.EmptyBins != 0 {
		t.Errorf("empty buckets = %d, want 0 — the hours are consecutive", set.EmptyBins)
	}
}
