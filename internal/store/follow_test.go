package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GrantPukka/loupe/internal/schema"
	"github.com/GrantPukka/loupe/internal/source"
)

// walker resolves a directory the way the CLI does on each poll.
func walker(dir string) func() ([]source.Source, error) {
	return func() ([]source.Source, error) { return source.Walk(dir, nil) }
}

// followed opens a directory through the cache, promotes, and returns a
// Follower over it — the state the CLI is in when --follow starts polling.
func followed(t *testing.T, dir, cacheDir string) (*Cached, *Follower) {
	t.Helper()
	ctx := context.Background()

	cached := openCached(t, dir, cacheDir, CacheOptions{})
	if _, _, err := cached.DB.InferAndPromote(ctx, schema.Options{}); err != nil {
		t.Fatalf("InferAndPromote: %v", err)
	}

	f, err := cached.DB.NewFollower(ctx, walker(dir), LoadOptions{})
	if err != nil {
		t.Fatalf("NewFollower: %v", err)
	}
	return cached, f
}

// A quiet poll must add nothing. Following a directory nobody is writing to
// should be indistinguishable from not following it.
func TestPollWithNoChangesAddsNothing(t *testing.T) {
	dir := logDir(t)
	cached, f := followed(t, dir, t.TempDir())
	ctx := context.Background()

	before, err := cached.DB.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}

	for i := 0; i < 3; i++ {
		batch, err := f.Poll(ctx)
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		if batch.Records != 0 {
			t.Errorf("quiet poll %d added %d records", i, batch.Records)
		}
	}

	after, err := cached.DB.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if after != before {
		t.Errorf("polling changed the record count from %d to %d", before, after)
	}
}

// The point of follow mode: lines written after the load show up, exactly once,
// and are selectable by the sequence range the batch reports.
func TestPollPicksUpAppendedRecords(t *testing.T) {
	dir := logDir(t)
	cached, f := followed(t, dir, t.TempDir())
	ctx := context.Background()

	before, err := cached.DB.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}

	appendTo(t, dir, "app.log",
		`{"ts":"2026-08-13T14:00:05Z","level":"error","msg":"live one","status":503}`,
		`{"ts":"2026-08-13T14:00:06Z","level":"info","msg":"live two","status":200}`)

	batch, err := f.Poll(ctx)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if batch.Records != 2 {
		t.Fatalf("poll added %d records, want 2", batch.Records)
	}

	after, err := cached.DB.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if after != before+2 {
		t.Errorf("table holds %d records, want %d", after, before+2)
	}

	// The batch's predicate must select exactly the new records, which is how
	// the CLI decides what to print. A bare seq range would also catch the
	// boundary record re-read to complete it, printing a duplicate line.
	where, args := batch.Predicate()
	rows, err := cached.DB.Query(ctx,
		`SELECT message FROM logs WHERE `+where+` ORDER BY seq`, args...)
	if err != nil {
		t.Fatalf("select new: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, m)
	}
	if len(got) != 2 || got[0] != "live one" || got[1] != "live two" {
		t.Errorf("new records = %v, want [live one, live two]", got)
	}

	// A second poll with nothing new must not re-emit them.
	again, err := f.Poll(ctx)
	if err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	if again.Records != 0 {
		t.Errorf("second poll re-added %d records", again.Records)
	}
}

// Live records must be filterable on promoted columns. If they arrive with the
// promoted columns NULL, status:>=500 silently misses them — the tool would be
// showing the incident while the filter denies it exists.
func TestPolledRecordsPopulatePromotedColumns(t *testing.T) {
	dir := logDir(t)
	cached, f := followed(t, dir, t.TempDir())
	ctx := context.Background()

	promos, err := cached.DB.Promotions(ctx)
	if err != nil {
		t.Fatalf("Promotions: %v", err)
	}
	var statusCol string
	for _, p := range promos {
		if p.Field == "status" {
			statusCol = p.Column
		}
	}
	if statusCol == "" {
		t.Skip("status was not promoted in this fixture")
	}

	appendTo(t, dir, "app.log",
		`{"ts":"2026-08-13T14:00:05Z","level":"error","msg":"live 503","status":503}`)

	if _, err := f.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	var got *int64
	err = cached.DB.QueryRow(ctx,
		fmt.Sprintf(`SELECT %s FROM logs WHERE message = 'live 503'`, quoteIdent(statusCol))).Scan(&got)
	if err != nil {
		t.Fatalf("read promoted column: %v", err)
	}
	if got == nil || *got != 503 {
		t.Errorf("promoted status = %v, want 503; live records are not filterable on it", got)
	}
}

// A file that appears after the session started must be picked up whole.
func TestPollPicksUpANewFile(t *testing.T) {
	dir := logDir(t)
	cached, f := followed(t, dir, t.TempDir())
	ctx := context.Background()

	before, err := cached.DB.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}

	writeFile(t, dir, "other.log",
		`{"ts":"2026-08-13T14:00:09Z","level":"warn","msg":"from a new file"}`+"\n")

	// The follower re-walks on every poll, so a file created after the session
	// started is picked up without rebuilding anything.
	batch, err := f.Poll(ctx)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if batch.Records != 1 {
		t.Errorf("poll added %d records from a new file, want 1", batch.Records)
	}

	after, err := cached.DB.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if after != before+1 {
		t.Errorf("table holds %d, want %d", after, before+1)
	}
}

// A record still being written when a poll runs must be completed by the next
// poll, not duplicated and not left truncated.
func TestPollCompletesARecordWrittenAcrossTicks(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "app.log",
		`{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"one"}`+"\n")

	cached, f := followed(t, dir, t.TempDir())
	ctx := context.Background()

	// A line arrives without its newline yet, as a partial write leaves it.
	partial := filepath.Join(dir, "app.log")
	fh, err := os.OpenFile(partial, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := fh.WriteString(`{"ts":"2026-08-13T14:00:01Z","level":"error","msg":"two"}`); err != nil {
		t.Fatalf("partial write: %v", err)
	}
	fh.Close()

	if _, err := f.Poll(ctx); err != nil {
		t.Fatalf("first Poll: %v", err)
	}

	// The rest of the line lands, followed by another record.
	appendTo(t, dir, "app.log", "", `{"ts":"2026-08-13T14:00:02Z","level":"info","msg":"three"}`)

	if _, err := f.Poll(ctx); err != nil {
		t.Fatalf("second Poll: %v", err)
	}

	var messages []string
	rows, err := cached.DB.Query(ctx, `SELECT message FROM logs ORDER BY seq`)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatalf("scan: %v", err)
		}
		messages = append(messages, m)
	}

	want := []string{"one", "two", "three"}
	if len(messages) != len(want) {
		t.Fatalf("messages = %v, want %v", messages, want)
	}
	for i := range want {
		if messages[i] != want[i] {
			t.Errorf("message %d = %q, want %q (full set %v)", i, messages[i], want[i], messages)
		}
	}
}
