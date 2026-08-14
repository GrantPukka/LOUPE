package store

import (
	"context"
	"strings"
	"testing"

	"github.com/VIGIL-OPS/loupe/internal/parse"
	"github.com/VIGIL-OPS/loupe/internal/schema"
)

// seeded builds a store with the given records already ingested.
func seeded(t *testing.T, src Source, entries ...parse.Entry) *DB {
	t.Helper()
	db := open(t)
	add(t, db, src, entries...)
	return db
}

func columnType(t *testing.T, db *DB, column string) string {
	t.Helper()

	var typ string
	err := db.QueryRow(context.Background(),
		`SELECT data_type FROM information_schema.columns
		 WHERE table_name = 'logs' AND column_name = ?`, column).Scan(&typ)
	if err != nil {
		t.Fatalf("column %q not found: %v", column, err)
	}
	return typ
}

func TestPromotionCreatesTypedColumns(t *testing.T) {
	var entries []parse.Entry
	for i := 0; i < 20; i++ {
		entries = append(entries, entry(int64(i+1), "2026-08-13T14:00:00Z", "info", "req", map[string]any{
			"status":     int64(200 + i),
			"latency_ms": float64(i) + 0.5,
			"trace_id":   "a91c40f2",
			"cached":     i%2 == 0,
		}))
	}

	db := seeded(t, Source{Name: "api", File: "api.log", Format: "jsonl"}, entries...)
	ctx := context.Background()

	promotions, _, err := db.InferAndPromote(ctx, schema.Options{})
	if err != nil {
		t.Fatalf("InferAndPromote: %v", err)
	}
	if len(promotions) != 4 {
		t.Fatalf("got %d promotions, want 4: %+v", len(promotions), promotions)
	}

	wantTypes := map[string]string{
		"status":     "BIGINT",
		"latency_ms": "DOUBLE",
		"trace_id":   "VARCHAR",
		"cached":     "BOOLEAN",
	}
	for column, want := range wantTypes {
		if got := columnType(t, db, column); got != want {
			t.Errorf("column %q is %s, want %s", column, got, want)
		}
	}

	// The values must actually be readable through the new columns.
	var n int64
	if err := db.QueryRow(ctx, `SELECT count(*) FROM logs WHERE status >= 210`).Scan(&n); err != nil {
		t.Fatalf("query promoted column: %v", err)
	}
	if n != 10 {
		t.Errorf("status >= 210 matched %d, want 10", n)
	}
}

// The check that matters. A bad cast does not error — TRY_CAST turns the value
// into NULL — so promotion can silently change what a filter returns.
func TestPromotionDoesNotChangeResults(t *testing.T) {
	var entries []parse.Entry
	for i := 0; i < 30; i++ {
		fields := map[string]any{
			"status": int64(200),
			"region": "eu-west-1",
		}
		if i%3 == 0 {
			fields["status"] = int64(502)
		}
		// Present in 70% of records: above the promotion threshold, but absent
		// from enough that "missing" must stay NULL rather than become zero.
		if i%10 < 7 {
			fields["latency_ms"] = int64(i * 10)
		}
		entries = append(entries, entry(int64(i+1), "2026-08-13T14:00:00Z", "info", "req", fields))
	}

	db := seeded(t, Source{Name: "api", File: "api.log", Format: "jsonl"}, entries...)
	ctx := context.Background()

	// The same questions, asked through JSON extraction, before promotion.
	type probe struct {
		name      string
		beforeSQL string
		afterSQL  string
	}
	probes := []probe{
		{
			"numeric comparison",
			`SELECT count(*) FROM logs WHERE TRY_CAST(fields->>'$.status' AS DOUBLE) >= 500`,
			`SELECT count(*) FROM logs WHERE TRY_CAST(status AS DOUBLE) >= 500`,
		},
		{
			"string equality",
			`SELECT count(*) FROM logs WHERE fields->>'$.region' = 'eu-west-1'`,
			`SELECT count(*) FROM logs WHERE region = 'eu-west-1'`,
		},
		{
			"absence",
			`SELECT count(*) FROM logs WHERE (fields->>'$.latency_ms') IS NULL`,
			`SELECT count(*) FROM logs WHERE latency_ms IS NULL`,
		},
	}

	before := make([]int64, len(probes))
	for i, p := range probes {
		if err := db.QueryRow(ctx, p.beforeSQL).Scan(&before[i]); err != nil {
			t.Fatalf("%s before: %v", p.name, err)
		}
	}

	if _, _, err := db.InferAndPromote(ctx, schema.Options{}); err != nil {
		t.Fatalf("InferAndPromote: %v", err)
	}

	for i, p := range probes {
		var after int64
		if err := db.QueryRow(ctx, p.afterSQL).Scan(&after); err != nil {
			t.Fatalf("%s after: %v", p.name, err)
		}
		if after != before[i] {
			t.Errorf("%s: %d rows before promotion, %d after — promotion changed the answer",
				p.name, before[i], after)
		}

		// The JSON path must keep working too, since promoted keys stay in the
		// bag and raw SQL may still use them.
		var stillJSON int64
		if err := db.QueryRow(ctx, p.beforeSQL).Scan(&stillJSON); err != nil {
			t.Fatalf("%s via json after promotion: %v", p.name, err)
		}
		if stillJSON != before[i] {
			t.Errorf("%s: the fields bag changed during promotion (%d vs %d)",
				p.name, stillJSON, before[i])
		}
	}
}

// Every record must survive the table rebuild.
func TestPromotionPreservesEveryRecord(t *testing.T) {
	var entries []parse.Entry
	for i := 0; i < 25; i++ {
		entries = append(entries, entry(int64(i+1), "2026-08-13T14:00:00Z", "info", "x",
			map[string]any{"status": int64(200)}))
	}
	// Records with no fields at all, and one no parser understood.
	entries = append(entries, entry(26, "2026-08-13T14:00:00Z", "info", "no fields", nil))
	entries = append(entries, parse.Entry{
		LineNo: 27, Raw: "junk", Parsed: false,
		Record: parse.Record{Message: "junk", Fields: map[string]any{}},
	})

	db := seeded(t, Source{Name: "api", File: "api.log", Format: "jsonl"}, entries...)
	ctx := context.Background()

	before, err := db.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}

	if _, _, err := db.InferAndPromote(ctx, schema.Options{}); err != nil {
		t.Fatalf("InferAndPromote: %v", err)
	}

	after, err := db.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if after != before {
		t.Errorf("%d records before promotion, %d after", before, after)
	}

	// The unparsed record must still carry its raw text.
	var raw string
	if err := db.QueryRow(ctx, `SELECT raw FROM logs WHERE NOT parsed`).Scan(&raw); err != nil {
		t.Fatalf("scan unparsed record: %v", err)
	}
	if raw != "junk" {
		t.Errorf("raw = %q after promotion", raw)
	}
}

// A value that does not fit the inferred type becomes NULL rather than failing
// the rebuild for every row.
func TestPromotionSurvivesAnUncastableValue(t *testing.T) {
	var entries []parse.Entry
	for i := 0; i < 20; i++ {
		entries = append(entries, entry(int64(i+1), "2026-08-13T14:00:00Z", "info", "x",
			map[string]any{"code": int64(200)}))
	}
	db := seeded(t, Source{Name: "api", File: "api.log", Format: "jsonl"}, entries...)
	ctx := context.Background()

	// One record whose code is not a number, appended after inference would
	// have sampled the rest.
	add(t, db, Source{Name: "api", File: "api.log", Format: "jsonl"},
		entry(21, "2026-08-13T14:00:00Z", "info", "x", map[string]any{"code": "not-a-number"}))

	promotions, _, err := db.InferAndPromote(ctx, schema.Options{SampleSize: 20})
	if err != nil {
		t.Fatalf("InferAndPromote: %v", err)
	}
	if len(promotions) == 0 {
		t.Fatal("nothing was promoted")
	}

	n, err := db.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 21 {
		t.Errorf("%d records after promotion, want 21 — a bad value cost us rows", n)
	}

	// The uncastable value is NULL in the column but intact in the bag.
	var nulls int64
	if err := db.QueryRow(ctx, `SELECT count(*) FROM logs WHERE code IS NULL`).Scan(&nulls); err != nil {
		t.Fatalf("count nulls: %v", err)
	}
	if nulls != 1 {
		t.Errorf("%d NULL codes, want 1", nulls)
	}

	var kept string
	err = db.QueryRow(ctx,
		`SELECT fields->>'$.code' FROM logs WHERE code IS NULL`).Scan(&kept)
	if err != nil {
		t.Fatalf("read the original value: %v", err)
	}
	if kept != "not-a-number" {
		t.Errorf("the original value was lost: %q", kept)
	}
}

// A cache hit reads the promotion decision back rather than re-sampling, so the
// query compiler sees the same columns a cold run produced.
func TestPromotionsRoundTripThroughTheDatabase(t *testing.T) {
	var entries []parse.Entry
	for i := 0; i < 20; i++ {
		entries = append(entries, entry(int64(i+1), "2026-08-13T14:00:00Z", "info", "x",
			map[string]any{"status": int64(200), "trace_id": "abc"}))
	}

	db := seeded(t, Source{Name: "api", File: "api.log", Format: "jsonl"}, entries...)
	ctx := context.Background()

	written, _, err := db.InferAndPromote(ctx, schema.Options{})
	if err != nil {
		t.Fatalf("InferAndPromote: %v", err)
	}

	read, err := db.Promotions(ctx)
	if err != nil {
		t.Fatalf("Promotions: %v", err)
	}
	if len(read) != len(written) {
		t.Fatalf("read %d promotions, wrote %d", len(read), len(written))
	}

	byField := map[string]schema.Promotion{}
	for _, p := range read {
		byField[p.Field] = p
	}
	for _, w := range written {
		got, ok := byField[w.Field]
		if !ok {
			t.Errorf("%q did not survive the round trip", w.Field)
			continue
		}
		if got.Column != w.Column || got.Kind != w.Kind {
			t.Errorf("%q came back as %s/%s, want %s/%s",
				w.Field, got.Column, got.Kind, w.Column, w.Kind)
		}
	}
}

// Promotions on a database that never ran inference must be empty, not an
// error, so an older cache file still opens.
func TestPromotionsWithoutInference(t *testing.T) {
	db := open(t)

	got, err := db.Promotions(context.Background())
	if err != nil {
		t.Fatalf("Promotions on a fresh database: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d promotions from a database that never ran inference", len(got))
	}
}

// A field name that is a SQL keyword or contains punctuation must not produce
// broken SQL.
func TestPromotionQuotesAwkwardNames(t *testing.T) {
	var entries []parse.Entry
	for i := 0; i < 20; i++ {
		entries = append(entries, entry(int64(i+1), "2026-08-13T14:00:00Z", "info", "x",
			map[string]any{
				"order":       int64(1),
				"select":      "x",
				"http.status": int64(200),
			}))
	}

	db := seeded(t, Source{Name: "api", File: "api.log", Format: "jsonl"}, entries...)
	ctx := context.Background()

	promotions, _, err := db.InferAndPromote(ctx, schema.Options{})
	if err != nil {
		t.Fatalf("InferAndPromote with awkward names: %v", err)
	}

	var columns []string
	for _, p := range promotions {
		columns = append(columns, p.Column)
	}
	joined := strings.Join(columns, ",")

	for _, want := range []string{"order", "select", "http_status"} {
		if !strings.Contains(joined, want) {
			t.Errorf("column %q missing from %v", want, columns)
		}
	}

	// And the table is still queryable.
	if _, err := db.Count(ctx); err != nil {
		t.Fatalf("table unusable after promoting keyword names: %v", err)
	}
}
