package session

import (
	"context"
	"fmt"
	"testing"
)

// foldFixture is a run of identical lines with a few distinct ones around it,
// which is what a retry storm looks like in a real log.
func foldFixture(t *testing.T) *Session {
	t.Helper()

	var lines []string
	add := func(ts int, msg string) {
		lines = append(lines, fmt.Sprintf(
			`{"ts":"2026-08-13T14:00:%02dZ","level":"info","msg":%q}`, ts, msg))
	}

	add(0, "starting up")
	for i := 1; i <= 20; i++ {
		// Differ only in the attempt number, which the templater masks, so
		// these are one shape and must fold together.
		add(i, fmt.Sprintf("connection refused, retry attempt %d", i))
	}
	add(21, "recovered")
	add(22, "connection refused, retry attempt 99")
	add(23, "shutting down")

	return openFixture(t, lines...)
}

func foldedRows(t *testing.T, sess *Session) [][]any {
	t.Helper()

	res, err := sess.Records(context.Background(), plan(t, sess, ""),
		RecordQuery{Sort: SortTime, Fold: true, Limit: -1})
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(res.Columns) == 0 || res.Columns[0] != "repeats" {
		t.Fatalf("columns = %v, want repeats first", res.Columns)
	}
	return res.Rows
}

// Twenty thousand identical lines are one fact, and printing them all buries
// the twenty on either side that are the reason anybody opened the file.
func TestFoldCollapsesConsecutiveRepeats(t *testing.T) {
	rows := foldedRows(t, foldFixture(t))

	// starting up, the run of 20, recovered, the lone retry, shutting down.
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5:\n%v", len(rows), rows)
	}

	repeats := make([]int64, len(rows))
	for i, row := range rows {
		repeats[i], _ = row[0].(int64)
	}
	want := []int64{1, 20, 1, 1, 1}
	for i := range want {
		if repeats[i] != want[i] {
			t.Fatalf("repeats = %v, want %v", repeats, want)
		}
	}
}

// Only consecutive runs fold. A count that pooled every occurrence in the file
// would be a different claim from "this happened N times in a row here", and
// the second is the one that helps at 4am.
func TestFoldDoesNotPoolNonConsecutiveRuns(t *testing.T) {
	rows := foldedRows(t, foldFixture(t))

	for _, row := range rows {
		if n, _ := row[0].(int64); n == 21 {
			t.Fatal("the lone later retry was pooled into the earlier run")
		}
	}
}

// The row shown is the first of its run, so its timestamp is when the run
// began rather than when it ended.
func TestFoldShowsTheFirstOfEachRun(t *testing.T) {
	rows := foldedRows(t, foldFixture(t))

	msg, _ := rows[1][4].(string)
	if msg != "connection refused, retry attempt 1" {
		t.Errorf("message = %q, want the first line of the run", msg)
	}
}

// Every record is still accounted for: folding hides repetition, never data.
func TestFoldLosesNoRecords(t *testing.T) {
	sess := foldFixture(t)

	total, err := sess.Count(context.Background(), plan(t, sess, ""))
	if err != nil {
		t.Fatalf("Count: %v", err)
	}

	var summed int64
	for _, row := range foldedRows(t, sess) {
		n, _ := row[0].(int64)
		summed += n
	}
	if summed != total {
		t.Errorf("folded rows account for %d records, want %d", summed, total)
	}
}
