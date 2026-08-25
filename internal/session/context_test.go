package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// contextFixture is ten numbered records with one interesting line in the
// middle, so that a window either side of it is easy to state exactly.
func contextFixture(t *testing.T) *Session {
	t.Helper()

	var lines []string
	for i := 0; i < 10; i++ {
		level, msg := "info", fmt.Sprintf("step %d", i)
		if i == 5 {
			level, msg = "error", "pool exhausted"
		}
		lines = append(lines, fmt.Sprintf(
			`{"ts":"2026-08-13T14:00:%02dZ","level":%q,"msg":%q}`, i, level, msg))
	}
	return openFixture(t, lines...)
}

// hitFlags reads the hit column out of a context listing, in row order.
func hitFlags(t *testing.T, sess *Session, filter string, n int) []bool {
	t.Helper()

	res, err := sess.Records(context.Background(), plan(t, sess, filter),
		RecordQuery{Sort: SortTime, Context: n, Limit: -1})
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(res.Columns) == 0 || res.Columns[0] != "hit" {
		t.Fatalf("columns = %v, want hit first", res.Columns)
	}

	out := make([]bool, 0, len(res.Rows))
	for _, row := range res.Rows {
		flag, _ := row[0].(bool)
		out = append(out, flag)
	}
	return out
}

// A stack trace is useless one frame at a time.
func TestRecordsWithContext(t *testing.T) {
	sess := contextFixture(t)

	got := hitFlags(t, sess, "level:error", 2)

	// Two either side of the single match, the match itself marked.
	want := []bool{false, false, true, false, false}
	if len(got) != len(want) {
		t.Fatalf("rows = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hit flags = %v, want %v", got, want)
		}
	}
}

// Zero context is the ordinary listing, with the ordinary columns.
func TestNoContextIsTheOrdinaryListing(t *testing.T) {
	sess := contextFixture(t)

	res, err := sess.Records(context.Background(), plan(t, sess, "level:error"),
		RecordQuery{Sort: SortTime, Limit: -1})
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if res.RowCount() != 1 {
		t.Errorf("rows = %d, want 1", res.RowCount())
	}
	if len(res.Columns) > 0 && res.Columns[0] == "hit" {
		t.Error("a listing with no context should not carry a hit column")
	}
}

// A window running off the start or end of the file is clipped, not an error.
func TestContextClipsAtTheEdges(t *testing.T) {
	sess := contextFixture(t)

	// step 0 is the first record, so there is nothing before it.
	got := hitFlags(t, sess, `"step 0"`, 3)
	if len(got) != 4 || !got[0] {
		t.Errorf("hit flags = %v, want the match first and three after it", got)
	}
}

// Overlapping windows produce one listing, not the same record twice.
func TestOverlappingContextDoesNotDuplicate(t *testing.T) {
	sess := contextFixture(t)

	res, err := sess.Records(context.Background(), plan(t, sess, "level:info"),
		RecordQuery{Sort: SortTime, Context: 5, Limit: -1})
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if res.RowCount() != 10 {
		t.Errorf("rows = %d, want 10 — every record once", res.RowCount())
	}
}

// Context must not leak across files: the record before the first line of one
// file is not the last line of another.
func TestContextDoesNotCrossFiles(t *testing.T) {
	sess := twoFileFixture(t)

	res, err := sess.Records(context.Background(), plan(t, sess, `"only match"`),
		RecordQuery{Sort: SortTime, Context: 5, Limit: -1})
	if err != nil {
		t.Fatalf("Records: %v", err)
	}

	for _, row := range res.Rows {
		if src, _ := row[4].(string); src != "b" {
			t.Errorf("context reached into %q; the match is in b alone", src)
		}
	}
}

// twoFileFixture puts the only match at the very start of the second file, so
// that a context window reaching backwards would land in the first.
func twoFileFixture(t *testing.T) *Session {
	t.Helper()
	dir := t.TempDir()

	write := func(name string, lines ...string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	write("a.log",
		`{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"a one"}`,
		`{"ts":"2026-08-13T14:00:01Z","level":"info","msg":"a two"}`,
		`{"ts":"2026-08-13T14:00:02Z","level":"info","msg":"a three"}`)
	write("b.log",
		`{"ts":"2026-08-13T14:00:03Z","level":"error","msg":"only match"}`,
		`{"ts":"2026-08-13T14:00:04Z","level":"info","msg":"b two"}`)

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
