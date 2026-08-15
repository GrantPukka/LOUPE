package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openFixture(t *testing.T, lines ...string) *Session {
	t.Helper()
	dir := t.TempDir()

	if len(lines) == 0 {
		lines = []string{
			`{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"a","status":200}`,
			`{"ts":"2026-08-13T14:05:00Z","level":"error","msg":"b","status":502}`,
			`{"ts":"2026-08-13T14:10:00Z","level":"warn","msg":"c","status":429}`,
			`{"level":"error","msg":"no timestamp","status":500}`,
			`not json at all`,
		}
	}

	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	sess, err := Open(context.Background(), Options{
		Paths:    []string{dir},
		Location: time.UTC,
		NoCache:  true,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { sess.Close() })

	return sess
}

func plan(t *testing.T, s *Session, filter string) Plan {
	t.Helper()
	p, err := s.Plan(context.Background(), filter)
	if err != nil {
		t.Fatalf("Plan(%q): %v", filter, err)
	}
	return p
}

func TestPlanAndRecords(t *testing.T) {
	s := openFixture(t)
	ctx := context.Background()

	res, err := s.Records(ctx, plan(t, s, "level:>=warn"), RecordQuery{})
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(res.Rows) != 3 {
		t.Errorf("got %d rows, want 3", len(res.Rows))
	}
}

func TestRecordsPaging(t *testing.T) {
	s := openFixture(t)
	ctx := context.Background()
	p := plan(t, s, "")

	first, err := s.Records(ctx, p, RecordQuery{Limit: 2})
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	second, err := s.Records(ctx, p, RecordQuery{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("Records with offset: %v", err)
	}

	if len(first.Rows) != 2 || len(second.Rows) != 2 {
		t.Fatalf("got %d and %d rows", len(first.Rows), len(second.Rows))
	}
	if first.Rows[0][3] == second.Rows[0][3] {
		t.Error("the offset page starts with the same record")
	}
}

// Records with no timestamp sort last rather than first, so they do not crowd
// the top of every unfiltered view.
func TestUntimestampedRecordsSortLast(t *testing.T) {
	s := openFixture(t)

	res, err := s.Records(context.Background(), plan(t, s, ""), RecordQuery{})
	if err != nil {
		t.Fatalf("Records: %v", err)
	}

	if res.Rows[0][0] == nil {
		t.Error("a record with no timestamp sorted first")
	}
	if res.Rows[len(res.Rows)-1][0] != nil {
		t.Error("the last row has a timestamp; untimestamped records should sort last")
	}
}

func TestHistogramAccountsForEveryRecord(t *testing.T) {
	s := openFixture(t)

	hist, err := s.Histogram(context.Background(), plan(t, s, ""), HistogramQuery{Buckets: 5})
	if err != nil {
		t.Fatalf("Histogram: %v", err)
	}

	var summed int64
	for _, b := range hist.Buckets {
		summed += b.Count
	}
	if summed != hist.Total {
		t.Errorf("buckets sum to %d, Total is %d", summed, hist.Total)
	}

	// Two records cannot be placed on a timeline, and a histogram that omits
	// them without saying so understates the data.
	if hist.NoTimestamp != 2 {
		t.Errorf("NoTimestamp = %d, want 2", hist.NoTimestamp)
	}
	if summed+hist.NoTimestamp != 5 {
		t.Errorf("%d bucketed + %d untimestamped, want 5 records accounted for",
			summed, hist.NoTimestamp)
	}
}

// Quiet intervals still get a bucket. Omitting them compresses time and makes a
// burst look continuous.
func TestHistogramIncludesEmptyBuckets(t *testing.T) {
	s := openFixture(t,
		`{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"a"}`,
		`{"ts":"2026-08-13T15:00:00Z","level":"info","msg":"b"}`,
	)

	hist, err := s.Histogram(context.Background(), plan(t, s, ""), HistogramQuery{
		Interval: 10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Histogram: %v", err)
	}

	if len(hist.Buckets) < 6 {
		t.Fatalf("got %d buckets over an hour of 10m intervals", len(hist.Buckets))
	}

	var empty int
	for _, b := range hist.Buckets {
		if b.Count == 0 {
			empty++
		}
	}
	if empty == 0 {
		t.Error("no empty buckets; the quiet hour was compressed away")
	}
}

// The level breakdown colours the timeline, so it must sum to the count.
func TestHistogramLevelsSumToCount(t *testing.T) {
	s := openFixture(t)

	hist, err := s.Histogram(context.Background(), plan(t, s, ""), HistogramQuery{Buckets: 10})
	if err != nil {
		t.Fatalf("Histogram: %v", err)
	}

	for _, b := range hist.Buckets {
		var summed int64
		for _, n := range b.Levels {
			summed += n
		}
		if summed != b.Count {
			t.Errorf("bucket %s: levels sum to %d, count is %d", b.Start, summed, b.Count)
		}
	}
}

// A time filter's window bounds the histogram, so dragging a range and
// re-querying shows exactly that range.
func TestHistogramUsesTheFilterWindow(t *testing.T) {
	s := openFixture(t)

	p := plan(t, s, "between:2026-08-13T14:00:00Z-2026-08-13T14:06:00Z")
	hist, err := s.Histogram(context.Background(), p, HistogramQuery{Buckets: 6})
	if err != nil {
		t.Fatalf("Histogram: %v", err)
	}

	if !hist.Start.Equal(p.Resolution.Interval.Start) {
		t.Errorf("Start = %v, want the filter's %v", hist.Start, p.Resolution.Interval.Start)
	}
	if !hist.End.Equal(p.Resolution.Interval.End) {
		t.Errorf("End = %v, want the filter's %v", hist.End, p.Resolution.Interval.End)
	}
	if hist.Total != 2 {
		t.Errorf("Total = %d, want the 2 records inside the window", hist.Total)
	}
}

func TestHistogramOnNoTimestampedRecords(t *testing.T) {
	s := openFixture(t, `{"level":"info","msg":"no timestamp anywhere"}`)

	hist, err := s.Histogram(context.Background(), plan(t, s, ""), HistogramQuery{})
	if err != nil {
		t.Fatalf("Histogram: %v", err)
	}
	if len(hist.Buckets) != 0 {
		t.Errorf("got %d buckets with nothing to plot", len(hist.Buckets))
	}
}

func TestBucketWidthRoundsToRecognisableUnits(t *testing.T) {
	tests := []struct {
		window  time.Duration
		buckets int
		want    time.Duration
	}{
		{time.Hour, 60, time.Minute},
		{24 * time.Hour, 24, time.Hour},
		{10 * time.Minute, 60, 10 * time.Second},
		{time.Minute, 60, time.Second},
		{7 * 24 * time.Hour, 7, 24 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.want.String(), func(t *testing.T) {
			if got := bucketWidth(tt.window, tt.buckets); got != tt.want {
				t.Errorf("bucketWidth(%v, %d) = %v, want %v", tt.window, tt.buckets, got, tt.want)
			}
		})
	}
}

func TestExplainOutsideWindow(t *testing.T) {
	s := openFixture(t)

	p := plan(t, s, "on:2020-01-01")
	got := s.Explain(context.Background(), p)

	if !got.OutsideWindow {
		t.Error("a window missing the data entirely was not identified")
	}
	if !strings.Contains(got.Text, "data covers") {
		t.Errorf("Text = %q, want it to say what the data covers", got.Text)
	}
}

func TestExplainNamesTheBarrenTerm(t *testing.T) {
	s := openFixture(t)

	p := plan(t, s, "level:error status:>=999")
	got := s.Explain(context.Background(), p)

	if len(got.BarrenTerms) != 1 || got.BarrenTerms[0] != "status:>=999" {
		t.Errorf("BarrenTerms = %v, want just status:>=999", got.BarrenTerms)
	}
}

func TestExplainCombination(t *testing.T) {
	s := openFixture(t)

	// Both terms match something alone, but nothing together.
	p := plan(t, s, "level:info status:502")
	got := s.Explain(context.Background(), p)

	if len(got.BarrenTerms) != 0 {
		t.Errorf("BarrenTerms = %v, want none — each term matches alone", got.BarrenTerms)
	}
	if !strings.Contains(got.Text, "combination") {
		t.Errorf("Text = %q, want it to blame the combination", got.Text)
	}
}

func TestSchemaIncludesPromotedFields(t *testing.T) {
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, `{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"x","status":200}`)
	}
	s := openFixture(t, lines...)

	sch, err := s.Schema(context.Background())
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}

	if sch.Promoted["status"] == "" {
		t.Errorf("status was not promoted: %v", sch.Promoted)
	}
	if len(sch.Sources) != 1 || sch.Sources[0] != "app" {
		t.Errorf("Sources = %v, want [app]", sch.Sources)
	}
}

func TestNoSourcesErrorNamesTheSkips(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty.log"), nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := Open(context.Background(), Options{Paths: []string{dir}, NoCache: true})
	if err == nil {
		t.Fatal("opening a directory of unreadable files succeeded")
	}

	var none NoSourcesError
	if v, ok := err.(NoSourcesError); ok {
		none = v
	} else {
		t.Fatalf("error type = %T, want NoSourcesError", err)
	}
	if len(none.Skipped) == 0 {
		t.Error("the skipped files were not reported")
	}
}
