// Package query_test executes compiled SQL against a real DuckDB.
//
// It is an external test package so that production code in query keeps no
// dependency on store; only the test needs both.
//
// CLAUDE.md lists "tests against a mocked store that pass while the real SQL is
// invalid" as a known way to make this codebase worse. Every predicate the
// compiler can emit is run here against an actual database with actual rows.
package query_test

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GrantPukka/loupe/internal/parse"
	"github.com/GrantPukka/loupe/internal/query"
	"github.com/GrantPukka/loupe/internal/store"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

type row struct {
	ts      string
	level   string
	message string
	source  string
	file    string
	format  string
	fields  map[string]any
	parsed  bool
}

// fixture builds a small but deliberately awkward dataset: mixed sources, a
// record with no level, a record with no timestamp, an unparsed line, and a
// level outside the canonical set.
func fixture() []row {
	return []row{
		{ts: "2026-08-13T14:00:00Z", level: "info", message: "request completed", source: "checkout-api", file: "logs/checkout-api.log", format: "jsonl", parsed: true,
			fields: map[string]any{"status": int64(200), "latency_ms": int64(42), "trace_id": "a91c40f2", "region": "eu-west-1"}},
		{ts: "2026-08-13T14:01:00Z", level: "warn", message: "rate limited by upstream", source: "checkout-api", file: "logs/checkout-api.log", format: "jsonl", parsed: true,
			fields: map[string]any{"status": int64(429), "latency_ms": int64(310), "trace_id": "b7712def"}},
		{ts: "2026-08-13T14:02:00Z", level: "error", message: "upstream timeout contacting payments", source: "checkout-api", file: "logs/checkout-api.log", format: "jsonl", parsed: true,
			fields: map[string]any{"status": int64(502), "latency_ms": int64(3400), "trace_id": "a91c40f2", "region": "us-east-1"}},
		{ts: "2026-08-13T14:03:00Z", level: "fatal", message: "FATAL: remaining connection slots are reserved", source: "postgres", file: "logs/postgresql.log", format: "postgres", parsed: true,
			fields: map[string]any{"pid": int64(20044)}},
		{ts: "2026-08-13T14:04:00Z", level: "debug", message: "evaluating feature flag", source: "checkout-api", file: "logs/checkout-api.log", format: "jsonl", parsed: true,
			fields: map[string]any{"flag": "new_checkout_flow"}},
		{ts: "2026-08-13T14:05:00Z", level: "info", message: "GET /api/checkout", source: "nginx", file: "logs/access.log", format: "nginx", parsed: true,
			fields: map[string]any{"status": int64(200), "path": "/api/checkout"}},
		{ts: "2026-08-13T13:00:00Z", level: "info", message: "GET /healthz", source: "nginx", file: "logs/access.log.1", format: "nginx", parsed: true,
			fields: map[string]any{"status": int64(200), "path": "/healthz"}},
		// No level at all. -level:debug must still return this.
		{ts: "2026-08-13T14:06:00Z", message: "no level on this record", source: "auth-svc", file: "logs/auth-svc.log", format: "logfmt", parsed: true,
			fields: map[string]any{"issuer": "auth.internal"}},
		// A level outside the canonical set. level:>=warn must not match it.
		{ts: "2026-08-13T14:07:00Z", level: "audit", message: "key rotated", source: "auth-svc", file: "logs/auth-svc.log", format: "logfmt", parsed: true,
			fields: map[string]any{"actor": "svc-rotate"}},
		// No timestamp. ts:none must find it.
		{level: "error", message: "truncated line with no timestamp", source: "checkout-api", file: "logs/checkout-api.log", format: "jsonl", parsed: false},
		// Uppercase in the message, for smart-case checks.
		{ts: "2026-08-13T14:08:00Z", level: "error", message: "Timeout waiting for lock", source: "postgres", file: "logs/postgresql.log", format: "postgres", parsed: true,
			fields: map[string]any{"pid": int64(20099)}},
	}
}

func setup(t *testing.T) (*store.DB, query.Schema) {
	t.Helper()

	db, err := store.Open("")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ing, err := db.NewIngester()
	if err != nil {
		t.Fatalf("new ingester: %v", err)
	}

	for i, r := range fixture() {
		ing.SetSource(store.Source{Name: r.source, File: r.file, Format: r.format})

		e := parse.Entry{
			LineNo: int64(i + 1),
			Raw:    r.message,
			Parsed: r.parsed,
			Record: parse.Record{Level: r.level, Message: r.message, Fields: r.fields},
		}
		if r.ts != "" {
			e.Timestamp = ts(r.ts)
			e.TimestampZoned = true
		}
		if err := ing.Add(e); err != nil {
			t.Fatalf("add row %d: %v", i, err)
		}
	}
	if err := ing.Close(); err != nil {
		t.Fatalf("close ingester: %v", err)
	}

	fields, err := db.Fields(context.Background())
	if err != nil {
		t.Fatalf("fields: %v", err)
	}
	sources, err := db.Sources(context.Background())
	if err != nil {
		t.Fatalf("sources: %v", err)
	}

	schema := query.Schema{Fields: fields}
	seen := map[string]bool{}
	for _, s := range sources {
		if !seen[s.Name] {
			seen[s.Name] = true
			schema.Sources = append(schema.Sources, s.Name)
		}
	}
	sort.Strings(schema.Sources)

	return db, schema
}

// run compiles a filter and executes it, returning the matched messages.
func run(t *testing.T, db *store.DB, schema query.Schema, filter string) []string {
	t.Helper()

	q, err := query.Parse(filter)
	if err != nil {
		t.Fatalf("Parse(%q): %v", filter, err)
	}
	sql, err := query.Compile(q, schema)
	if err != nil {
		t.Fatalf("Compile(%q): %v", filter, err)
	}

	rows, err := db.Query(context.Background(),
		`SELECT message FROM logs WHERE `+sql.Where+` ORDER BY seq`, sql.Args...)
	if err != nil {
		t.Fatalf("executing %q\n  SQL:  %s\n  args: %#v\n  err:  %v",
			filter, sql.Where, sql.Args, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var msg string
		if err := rows.Scan(&msg); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, msg)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func TestFiltersAgainstRealDuckDB(t *testing.T) {
	db, schema := setup(t)

	tests := []struct {
		name   string
		filter string
		want   []string
	}{
		{
			name:   "exact level",
			filter: "level:error",
			want: []string{
				"upstream timeout contacting payments",
				"truncated line with no timestamp",
				"Timeout waiting for lock",
			},
		},
		{
			name:   "level list",
			filter: "level:warn,fatal",
			want: []string{
				"rate limited by upstream",
				"FATAL: remaining connection slots are reserved",
			},
		},
		{
			name:   "ordinal comparison",
			filter: "level:>=warn",
			want: []string{
				"rate limited by upstream",
				"upstream timeout contacting payments",
				"FATAL: remaining connection slots are reserved",
				"truncated line with no timestamp",
				"Timeout waiting for lock",
			},
		},
		{
			// The record with no level and the one with a custom level must
			// both survive the exclusion.
			name:   "negated level keeps records with no level",
			filter: "-level:debug",
			want: []string{
				"request completed",
				"rate limited by upstream",
				"upstream timeout contacting payments",
				"FATAL: remaining connection slots are reserved",
				"GET /api/checkout",
				"GET /healthz",
				"no level on this record",
				"key rotated",
				"truncated line with no timestamp",
				"Timeout waiting for lock",
			},
		},
		{
			name:   "source",
			filter: "source:postgres",
			want: []string{
				"FATAL: remaining connection slots are reserved",
				"Timeout waiting for lock",
			},
		},
		{
			name:   "source prefix",
			filter: "source:check",
			want: []string{
				"request completed",
				"rate limited by upstream",
				"upstream timeout contacting payments",
				"evaluating feature flag",
				"truncated line with no timestamp",
			},
		},
		{
			name:   "negated source",
			filter: "-source:checkout-api -source:nginx",
			want: []string{
				"FATAL: remaining connection slots are reserved",
				"no level on this record",
				"key rotated",
				"Timeout waiting for lock",
			},
		},
		{
			name:   "file glob catches the rotation group",
			filter: "file:access.log*",
			want:   []string{"GET /api/checkout", "GET /healthz"},
		},
		{
			name:   "file exact matches the base name",
			filter: "file:postgresql.log",
			want: []string{
				"FATAL: remaining connection slots are reserved",
				"Timeout waiting for lock",
			},
		},
		{
			name:   "format",
			filter: "format:logfmt",
			want:   []string{"no level on this record", "key rotated"},
		},
		{
			name:   "numeric field comparison",
			filter: "status:>=500",
			want:   []string{"upstream timeout contacting payments"},
		},
		{
			// 9 must not sort above 10: this is what the cast is for.
			name:   "numeric ordering is numeric",
			filter: "latency_ms:>100",
			want: []string{
				"rate limited by upstream",
				"upstream timeout contacting payments",
			},
		},
		{
			name:   "field equality",
			filter: "trace_id:a91c40f2",
			want: []string{
				"request completed",
				"upstream timeout contacting payments",
			},
		},
		{
			name:   "field existence",
			filter: "trace_id:*",
			want: []string{
				"request completed",
				"rate limited by upstream",
				"upstream timeout contacting payments",
			},
		},
		{
			name:   "field absence",
			filter: "trace_id:none",
			want: []string{
				"FATAL: remaining connection slots are reserved",
				"evaluating feature flag",
				"GET /api/checkout",
				"GET /healthz",
				"no level on this record",
				"key rotated",
				"truncated line with no timestamp",
				"Timeout waiting for lock",
			},
		},
		{
			name:   "records with no timestamp",
			filter: "ts:none",
			want:   []string{"truncated line with no timestamp"},
		},
		{
			name:   "free text searches the message",
			filter: "timeout",
			want: []string{
				"upstream timeout contacting payments",
				"Timeout waiting for lock",
			},
		},
		{
			// Bare words search field values too, which is what people expect
			// from a search box.
			name:   "free text searches field values",
			filter: "eu-west-1",
			want:   []string{"request completed"},
		},
		{
			name:   "quoted phrase",
			filter: `"reserved"`,
			want:   []string{"FATAL: remaining connection slots are reserved"},
		},
		{
			name:   "negated free text",
			filter: "-timeout level:error",
			want:   []string{"truncated line with no timestamp"},
		},
		{
			name:   "substring on message only",
			filter: "message~upstream",
			want: []string{
				"rate limited by upstream",
				"upstream timeout contacting payments",
			},
		},
		{
			name:   "regex",
			filter: `message~/^GET \/api/`,
			want:   []string{"GET /api/checkout"},
		},
		{
			// Smart case: an uppercase character makes it case-sensitive.
			name:   "smart case is sensitive when the pattern has uppercase",
			filter: "message~Timeout",
			want:   []string{"Timeout waiting for lock"},
		},
		{
			name:   "terms compose with AND",
			filter: "source:checkout-api level:>=warn status:>=500",
			want:   []string{"upstream timeout contacting payments"},
		},
		{
			name:   "the spec's composed example",
			filter: "level:>=warn -source:nginx timeout",
			want: []string{
				"upstream timeout contacting payments",
				"Timeout waiting for lock",
			},
		},
		{
			name:   "empty query returns everything",
			filter: "",
			want: []string{
				"request completed",
				"rate limited by upstream",
				"upstream timeout contacting payments",
				"FATAL: remaining connection slots are reserved",
				"evaluating feature flag",
				"GET /api/checkout",
				"GET /healthz",
				"no level on this record",
				"key rotated",
				"truncated line with no timestamp",
				"Timeout waiting for lock",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := run(t, db, schema, tt.filter)
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("filter %q matched:\n  %s\nwant:\n  %s",
					tt.filter, strings.Join(got, "\n  "), strings.Join(tt.want, "\n  "))
			}
		})
	}
}

// An unparsed record must remain findable by its text. Those are exactly the
// records someone is hunting for when the tool has otherwise let them down.
func TestUnparsedRecordsAreSearchable(t *testing.T) {
	db, schema := setup(t)

	got := run(t, db, schema, "truncated")
	if len(got) != 1 {
		t.Errorf("searching for text in an unparsed record matched %v, want 1 record", got)
	}
}

// A value containing SQL syntax must be treated as data all the way through to
// the database.
func TestInjectionIsInertAgainstTheRealDatabase(t *testing.T) {
	db, schema := setup(t)

	before, err := db.Count(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}

	got := run(t, db, schema, `trace_id:"' OR 1=1 --"`)
	if len(got) != 0 {
		t.Errorf("injection attempt matched %d rows, want 0", len(got))
	}

	after, err := db.Count(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if before != after {
		t.Errorf("row count changed from %d to %d", before, after)
	}
}

// Field names come out of log files, so they can contain anything at all.
// jsonPath embeds the name in a $."..." path that itself sits inside a '...'
// SQL string literal; until both contexts were escaped, filtering a field named
// a'b closed the literal early and DuckDB answered `syntax error at or near
// "b"` — a legitimate record was unqueryable.
func TestQuotesInFieldNamesAgainstRealDuckDB(t *testing.T) {
	names := []string{`a'b`, `e\f`, `it's`, `two''quotes`, `weird"key`, `a key with spaces`, `last`}

	db, err := store.Open("")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ing, err := db.NewIngester()
	if err != nil {
		t.Fatalf("new ingester: %v", err)
	}
	ing.SetSource(store.Source{Name: "api", File: "api.log", Format: "jsonl"})

	for i, n := range append(append([]string{}, names...), "plain") {
		msg := "match " + n
		if err := ing.Add(parse.Entry{
			LineNo: int64(i + 1),
			Raw:    msg,
			Parsed: true,
			Record: parse.Record{
				Timestamp:      ts("2026-08-13T14:00:00Z"),
				TimestampZoned: true,
				Level:          "info",
				Message:        msg,
				Fields:         map[string]any{n: "z"},
			},
		}); err != nil {
			t.Fatalf("add %q: %v", n, err)
		}
	}
	if err := ing.Close(); err != nil {
		t.Fatalf("close ingester: %v", err)
	}

	fields, err := db.Fields(context.Background())
	if err != nil {
		t.Fatalf("fields: %v", err)
	}
	schema := query.Schema{Fields: fields}

	// Rendering the term is how a user gets a filter for an awkward key in the
	// first place — the UI writes it into the filter box — so going through
	// String() here tests render, parse and compile as one path.
	for _, n := range names {
		term := &query.FieldTerm{Key: n, Values: []query.Value{{Text: "z"}}}
		filter := term.String()

		got := run(t, db, schema, filter)
		if len(got) != 1 || got[0] != "match "+n {
			t.Errorf("filter %q (key %q) matched %v, want [%q]", filter, n, got, "match "+n)
		}
	}
}

// Every operator must produce SQL DuckDB actually accepts. A predicate that
// compiles to a syntax error is caught here rather than by a user.
func TestEveryOperatorProducesValidSQL(t *testing.T) {
	db, schema := setup(t)

	filters := []string{
		"level:info", "level:error,fatal", "level:>=warn", "level:>warn",
		"level:<=info", "level:<info", "-level:debug",
		"source:nginx", "source:nginx,postgres", "-source:nginx",
		"file:access.log", "file:access.log*", "format:jsonl",
		"status:500", "status:>=500", "status:>100", "status:<=200", "status:<300",
		"latency_ms:>1000", "region:eu-west-1", "trace_id:*", "trace_id:none",
		"ts:none", "level:none", "raw~timeout", "line_no:>1",
		"timeout", `"read timed out"`, "-healthz",
		"message~timeout", "-message~healthz", `message~/^GET/`,
		`message~/[0-9]{3}/`, "path:/api/checkout",
		"level:>=warn -source:nginx timeout status:>=500",
	}

	for _, filter := range filters {
		t.Run(filter, func(t *testing.T) {
			run(t, db, schema, filter)
		})
	}
}
