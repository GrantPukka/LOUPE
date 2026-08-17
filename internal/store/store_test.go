package store

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GrantPukka/loupe/internal/parse"
	"github.com/GrantPukka/loupe/internal/source"
)

// open gives a real in-memory DuckDB. CLAUDE.md forbids mocking it: tests
// against a fake store pass while the actual SQL is invalid.
func open(t *testing.T) *DB {
	t.Helper()
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

// add ingests entries as one source.
func add(t *testing.T, db *DB, src Source, entries ...parse.Entry) {
	t.Helper()
	ing, err := db.NewIngester()
	if err != nil {
		t.Fatalf("NewIngester: %v", err)
	}
	ing.SetSource(src)
	for _, e := range entries {
		if err := ing.Add(e); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if err := ing.Close(); err != nil {
		t.Fatalf("Close ingester: %v", err)
	}
}

func entry(lineNo int64, timestamp, level, msg string, fields map[string]any) parse.Entry {
	e := parse.Entry{
		LineNo: lineNo,
		Raw:    msg,
		Parsed: true,
		Record: parse.Record{Level: level, Message: msg, Fields: fields},
	}
	if timestamp != "" {
		e.Timestamp = ts(timestamp)
		e.TimestampZoned = true
	}
	return e
}

func TestOpenCreatesSchema(t *testing.T) {
	db := open(t)

	n, err := db.Count(context.Background())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 0 {
		t.Errorf("Count = %d on a fresh store, want 0", n)
	}
}

func TestIngestAndQuery(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	add(t, db, Source{Name: "api", File: "api.log", Format: "jsonl"},
		entry(1, "2026-08-13T14:00:00Z", "info", "started", map[string]any{"port": int64(8080)}),
		entry(2, "2026-08-13T14:00:01Z", "error", "boom", map[string]any{"status": int64(500)}),
		entry(3, "2026-08-13T14:00:02Z", "error", "boom again", nil),
	)

	n, err := db.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 3 {
		t.Fatalf("Count = %d, want 3", n)
	}

	res, err := db.QueryResult(ctx, 0,
		`SELECT level, count(*) FROM logs GROUP BY 1 ORDER BY 1`)
	if err != nil {
		t.Fatalf("QueryResult: %v", err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("got %d level groups, want 2", len(res.Rows))
	}
	if got := res.Rows[0][0]; got != "error" {
		t.Errorf("first level = %v, want error", got)
	}
	if got := res.Rows[0][1]; got != int64(2) {
		t.Errorf("error count = %v, want 2", got)
	}
}

// A record with no timestamp must be stored as NULL, not as the year 1. A zero
// value would sort it before everything and quietly pollute every time window.
func TestMissingTimestampIsNullNotZero(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	add(t, db, Source{Name: "api", File: "api.log", Format: "jsonl"},
		entry(1, "2026-08-13T14:00:00Z", "info", "has one", nil),
		entry(2, "", "info", "has none", nil),
	)

	var nulls int64
	if err := db.QueryRow(ctx, `SELECT count(*) FROM logs WHERE ts IS NULL`).Scan(&nulls); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if nulls != 1 {
		t.Errorf("got %d NULL timestamps, want 1", nulls)
	}

	// And it must not be swept into a range comparison.
	var inRange int64
	err := db.QueryRow(ctx,
		`SELECT count(*) FROM logs WHERE ts >= ? AND ts < ?`,
		ts("2000-01-01T00:00:00Z"), ts("2030-01-01T00:00:00Z")).Scan(&inRange)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if inRange != 1 {
		t.Errorf("range matched %d records, want 1; a NULL timestamp leaked into a window", inRange)
	}

	// ts:none must be able to find it.
	var none int64
	if err := db.QueryRow(ctx, `SELECT count(*) FROM logs WHERE ts IS NULL`).Scan(&none); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if none != 1 {
		t.Errorf("ts:none equivalent found %d, want 1", none)
	}
}

// Raw is kept for every record, whether or not a parser understood it. A
// handoff includes it because the receiver may not trust our parser.
func TestRawIsStoredForUnparsedRecords(t *testing.T) {
	db := open(t)

	const junk = "!!! not a log line !!!"
	add(t, db, Source{Name: "api", File: "api.log", Format: "jsonl"}, parse.Entry{
		LineNo: 1,
		Raw:    junk,
		Parsed: false,
		Record: parse.Record{Message: junk, Fields: map[string]any{}},
	})

	var raw string
	var parsed bool
	err := db.QueryRow(context.Background(),
		`SELECT raw, parsed FROM logs`).Scan(&raw, &parsed)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if raw != junk {
		t.Errorf("raw = %q, want %q", raw, junk)
	}
	if parsed {
		t.Error("parsed = true for an unparsed record")
	}
}

func TestFieldsAreQueryableJSON(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	add(t, db, Source{Name: "api", File: "api.log", Format: "jsonl"},
		entry(1, "2026-08-13T14:00:00Z", "error", "a", map[string]any{
			"status": int64(502), "trace_id": "a91c40f2", "latency_ms": int64(3400),
		}),
		entry(2, "2026-08-13T14:00:01Z", "info", "b", map[string]any{
			"status": int64(200), "trace_id": "b7712def",
		}),
	)

	var n int64
	err := db.QueryRow(ctx,
		`SELECT count(*) FROM logs WHERE CAST(fields->>'$.status' AS BIGINT) >= ?`, 500).Scan(&n)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != 1 {
		t.Errorf("status>=500 matched %d, want 1", n)
	}

	var trace string
	err = db.QueryRow(ctx,
		`SELECT fields->>'$.trace_id' FROM logs WHERE level = ?`, "error").Scan(&trace)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if trace != "a91c40f2" {
		t.Errorf("trace_id = %q", trace)
	}
}

// An empty fields bag is NULL rather than "{}", so field:none can be answered
// without parsing JSON.
func TestEmptyFieldsIsNull(t *testing.T) {
	db := open(t)

	add(t, db, Source{Name: "api", File: "api.log", Format: "jsonl"},
		entry(1, "2026-08-13T14:00:00Z", "info", "no fields", nil),
		entry(2, "2026-08-13T14:00:01Z", "info", "no fields either", map[string]any{}),
	)

	var nulls int64
	err := db.QueryRow(context.Background(),
		`SELECT count(*) FROM logs WHERE fields IS NULL`).Scan(&nulls)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if nulls != 2 {
		t.Errorf("got %d NULL field bags, want 2", nulls)
	}
}

func TestFieldsListsDistinctKeys(t *testing.T) {
	db := open(t)

	add(t, db, Source{Name: "api", File: "api.log", Format: "jsonl"},
		entry(1, "2026-08-13T14:00:00Z", "info", "a", map[string]any{"status": int64(200), "path": "/x"}),
		entry(2, "2026-08-13T14:00:01Z", "info", "b", map[string]any{"status": int64(500), "user_id": "u_1"}),
	)

	got, err := db.Fields(context.Background())
	if err != nil {
		t.Fatalf("Fields: %v", err)
	}

	want := map[string]bool{"status": true, "path": true, "user_id": true}
	if len(got) != len(want) {
		t.Fatalf("Fields() = %v, want %d keys", got, len(want))
	}
	for _, k := range got {
		if !want[k] {
			t.Errorf("unexpected field key %q", k)
		}
	}
}

func TestTimeRange(t *testing.T) {
	db := open(t)

	add(t, db, Source{Name: "api", File: "api.log", Format: "jsonl"},
		entry(1, "2026-08-13T14:00:00Z", "info", "first", nil),
		entry(2, "2026-08-13T16:30:00Z", "info", "last", nil),
		entry(3, "", "info", "no timestamp", nil),
	)

	oldest, newest, noTS, err := db.TimeRange(context.Background())
	if err != nil {
		t.Fatalf("TimeRange: %v", err)
	}
	if !oldest.Equal(ts("2026-08-13T14:00:00Z")) {
		t.Errorf("oldest = %v", oldest)
	}
	if !newest.Equal(ts("2026-08-13T16:30:00Z")) {
		t.Errorf("newest = %v", newest)
	}
	if noTS != 1 {
		t.Errorf("noTimestamp = %d, want 1", noTS)
	}
}

// Truncated output must state the real total. Output that hides its own
// incompleteness is worse than no output.
func TestQueryResultReportsTruncation(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	var entries []parse.Entry
	for i := 0; i < 50; i++ {
		entries = append(entries, entry(int64(i+1), "2026-08-13T14:00:00Z", "info", "x", nil))
	}
	add(t, db, Source{Name: "api", File: "api.log", Format: "jsonl"}, entries...)

	res, err := db.QueryResult(ctx, 10, `SELECT seq FROM logs ORDER BY seq`)
	if err != nil {
		t.Fatalf("QueryResult: %v", err)
	}

	if !res.Truncated {
		t.Error("Truncated = false when a limit cut the output")
	}
	if res.RowCount() != 10 {
		t.Errorf("RowCount = %d, want 10", res.RowCount())
	}
	if res.Total != 50 {
		t.Errorf("Total = %d, want the full 50", res.Total)
	}

	full, err := db.QueryResult(ctx, 0, `SELECT seq FROM logs`)
	if err != nil {
		t.Fatalf("QueryResult: %v", err)
	}
	if full.Truncated {
		t.Error("Truncated = true with no limit")
	}
	if full.Total != 50 {
		t.Errorf("Total = %d, want 50", full.Total)
	}
}

func TestSourcesReportsTimezoneProvenance(t *testing.T) {
	db := open(t)

	// A format carrying its own offset.
	add(t, db, Source{Name: "api", File: "api.log", Format: "jsonl"},
		entry(1, "2026-08-13T14:00:00Z", "info", "zoned", nil),
	)

	// A format carrying none: every timestamp depends on an assumption.
	unzoned := entry(1, "2026-08-13T14:00:00Z", "info", "unzoned", nil)
	unzoned.TimestampZoned = false
	add(t, db, Source{Name: "worker", File: "worker.log", Format: "log4j"}, unzoned)

	// A file with no timestamps at all.
	add(t, db, Source{Name: "junk", File: "junk.log", Format: "text"},
		entry(1, "", "", "no timestamp here", nil),
	)

	infos, err := db.Sources(context.Background())
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if len(infos) != 3 {
		t.Fatalf("got %d sources, want 3", len(infos))
	}

	status := map[string]string{}
	for _, si := range infos {
		status[si.Name] = si.TimezoneStatus()
	}

	if got := status["api"]; got != "known (carried in the format)" {
		t.Errorf("api timezone status = %q", got)
	}
	if got := status["worker"]; got != "assumed — no offset in format" {
		t.Errorf("worker timezone status = %q", got)
	}
	// Claiming a known timezone for a file with no timestamps would be a
	// reassuring lie.
	if got := status["junk"]; got != "n/a — no timestamps" {
		t.Errorf("junk timezone status = %q", got)
	}
}

// The end-to-end path: files on disk through detection, parsing, and ingest.
func TestLoadFromDisk(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, dir, "api.log",
		`{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"a","status":200}`+"\n"+
			`{"ts":"2026-08-13T14:00:01Z","level":"error","msg":"b","status":500}`+"\n"+
			`not json at all`+"\n")
	writeFile(t, dir, "auth.log",
		`ts=2026-08-13T14:00:02Z level=warn msg="c" user_id=u_1`+"\n")

	sources, err := source.Walk(dir, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	db := open(t)
	load, err := db.Load(context.Background(), sources, LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(load.Errors) != 0 {
		t.Fatalf("Load reported errors: %v", load.Errors)
	}
	if load.Stats.Records != 4 {
		t.Errorf("Records = %d, want 4", load.Stats.Records)
	}
	if load.Stats.Unparsed != 1 {
		t.Errorf("Unparsed = %d, want 1", load.Stats.Unparsed)
	}

	// Both formats must have been detected, not one applied to both.
	formats := map[string]bool{}
	for _, r := range load.Results {
		formats[r.Source.Format] = true
	}
	if !formats["jsonl"] || !formats["logfmt"] {
		t.Errorf("detected formats = %v, want both jsonl and logfmt", formats)
	}

	n, err := db.Count(context.Background())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 4 {
		t.Errorf("Count = %d, want 4", n)
	}
}

// One unreadable file must not stop the others being read.
func TestLoadContinuesPastAFailingSource(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "good.log", `{"ts":"2026-08-13T14:00:00Z","msg":"fine"}`+"\n")

	sources, err := source.Walk(dir, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	sources = append(sources, brokenSource{})

	db := open(t)
	load, err := db.Load(context.Background(), sources, LoadOptions{})
	if err != nil {
		t.Fatalf("Load returned a fatal error: %v", err)
	}

	if len(load.Errors) != 1 {
		t.Errorf("got %d errors, want 1", len(load.Errors))
	}
	if load.Stats.Records != 1 {
		t.Errorf("Records = %d; the good file was not read", load.Stats.Records)
	}
}

func TestLoadRejectsUnknownParser(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.log", "x\n")

	sources, _ := source.Walk(dir, nil)
	db := open(t)

	load, err := db.Load(context.Background(), sources, LoadOptions{Parser: "no-such-format"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(load.Errors) != 1 {
		t.Fatalf("got %d errors, want 1 naming the bad parser", len(load.Errors))
	}
}

func TestLogicalNameCollapsesRotationGroups(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"checkout-api.log", "checkout-api"},
		{"/var/log/checkout-api.log.1", "checkout-api"},
		{"/var/log/checkout-api.log.2.gz", "checkout-api"},
		{"syslog", "syslog"},
		{"access.log", "access"},
		{"app.out", "app"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := logicalName(tt.path); got != tt.want {
				t.Errorf("logicalName(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

// A cached database reopened must not restart the sequence, or new records
// collide with existing ones and ordering breaks.
func TestSequenceResumesOnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.duckdb")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	add(t, first, Source{Name: "api", File: "api.log", Format: "jsonl"},
		entry(1, "2026-08-13T14:00:00Z", "info", "a", nil),
		entry(2, "2026-08-13T14:00:01Z", "info", "b", nil),
	)
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()

	if second.seq != 2 {
		t.Errorf("seq = %d after reopening a store with 2 records, want 2", second.seq)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// brokenSource always fails to open.
type brokenSource struct{}

func (brokenSource) Name() string        { return "broken.log" }
func (brokenSource) Size() int64         { return 0 }
func (brokenSource) Fingerprint() string { return "" }
func (brokenSource) Open(context.Context) (io.ReadCloser, error) {
	return nil, os.ErrPermission
}

// Regression: the fields column must hold a JSON object, not a JSON string.
//
// Appending a Go string into a JSON-typed column double-encodes it. Nothing
// errors — json_type reports VARCHAR, every extraction returns NULL, and every
// field filter in the DSL silently matches nothing. A query that succeeds and
// returns the wrong answer is the worst outcome this codebase can produce, so
// this is pinned.
func TestFieldsAreNotDoubleEncoded(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	add(t, db, Source{Name: "api", File: "api.log", Format: "jsonl"},
		entry(1, "2026-08-13T14:00:00Z", "info", "a", map[string]any{"trace_id": "a91c40f2"}),
	)

	var jsonType string
	err := db.QueryRow(ctx, `SELECT json_type(fields) FROM logs`).Scan(&jsonType)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if jsonType != "OBJECT" {
		t.Errorf("json_type(fields) = %q, want OBJECT; the fields bag is double-encoded "+
			"and every field filter will silently match nothing", jsonType)
	}

	var keys string
	if err := db.QueryRow(ctx, `SELECT json_keys(fields)::VARCHAR FROM logs`).Scan(&keys); err != nil {
		t.Fatalf("json_keys: %v", err)
	}
	if keys != `[trace_id]` {
		t.Errorf("json_keys = %s, want [trace_id]", keys)
	}
}

// A source whose format carries no timezone is read under an assumption, and
// changing that assumption must move the instants.
//
// This is the trap in docs/FILTER-DSL.md section 2.5: if the server runs UTC
// and the operator's laptop is on BST, every record from such a source is
// displayed an hour out with nothing warning anybody.
func TestSourceTimezoneAssumptionChangesTheInstants(t *testing.T) {
	dir := t.TempDir()

	// A log4j-style line: local time, no offset anywhere in the format.
	writeFile(t, dir, "worker.log", "2026-08-13 14:00:00.000 [worker-1] ERROR c.a.p.Handler - read timed out\n")

	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	read := func(zones map[string]*time.Location) (time.Time, int64) {
		t.Helper()

		sources, err := source.Walk(dir, nil)
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}

		db := open(t)
		load, err := db.Load(context.Background(), sources, LoadOptions{SourceZones: zones})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}

		var ts time.Time
		if err := db.QueryRow(context.Background(), `SELECT ts FROM logs`).Scan(&ts); err != nil {
			t.Fatalf("scan: %v", err)
		}
		return ts, load.Stats.ZoneAssumed
	}

	utcInstant, assumedUTC := read(nil)
	tokyoInstant, assumedTokyo := read(map[string]*time.Location{"": tokyo})

	// The default is UTC, not local: servers overwhelmingly run UTC and the
	// wrong default here is worse than a slightly surprising one.
	if got := utcInstant.UTC().Format("15:04"); got != "14:00" {
		t.Errorf("default assumption produced %s UTC, want 14:00 — the default should be UTC", got)
	}

	// Tokyo is UTC+9, so the same wall clock is nine hours earlier in UTC.
	if diff := utcInstant.Sub(tokyoInstant); diff != 9*time.Hour {
		t.Errorf("difference = %v, want 9h; --source-tz was not applied", diff)
	}

	// Both runs must report that an assumption was made at all, or it is
	// invisible to the user.
	if assumedUTC == 0 || assumedTokyo == 0 {
		t.Errorf("ZoneAssumed = %d and %d; a zoneless source must report its assumption",
			assumedUTC, assumedTokyo)
	}
}

// A per-source --source-tz must beat the blanket default.
func TestPerSourceTimezoneOverridesTheDefault(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "worker.log", "2026-08-13 14:00:00.000 [worker-1] ERROR c.a.p.Handler - one\n")
	writeFile(t, dir, "other.log", "2026-08-13 14:00:00.000 [main] INFO c.a.Other - two\n")

	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	sources, err := source.Walk(dir, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	db := open(t)
	_, err = db.Load(context.Background(), sources, LoadOptions{
		SourceZones: map[string]*time.Location{"": time.UTC, "worker": tokyo},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var workerTS, otherTS time.Time
	ctx := context.Background()
	if err := db.QueryRow(ctx, `SELECT ts FROM logs WHERE source = 'worker'`).Scan(&workerTS); err != nil {
		t.Fatalf("scan worker: %v", err)
	}
	if err := db.QueryRow(ctx, `SELECT ts FROM logs WHERE source = 'other'`).Scan(&otherTS); err != nil {
		t.Fatalf("scan other: %v", err)
	}

	if diff := otherTS.Sub(workerTS); diff != 9*time.Hour {
		t.Errorf("difference = %v, want 9h; the named source did not override the default", diff)
	}
}
