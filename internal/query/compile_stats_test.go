package query

import (
	"strings"
	"testing"
	"time"
)

func statsSchema() Schema {
	return Schema{
		Fields:   []string{"latency_ms", "trace_id", "region"},
		Sources:  []string{"checkout-api", "nginx"},
		Promoted: map[string]string{"status": "f_status", "path": "f_path"},
	}
}

func compileStats(t *testing.T, filter string) StatsSQL {
	t.Helper()

	q, err := Parse(filter)
	if err != nil {
		t.Fatalf("Parse(%q): %v", filter, err)
	}
	sql, err := CompileStats(q.Stats, statsSchema(), StatsOptions{
		Origin: time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("CompileStats(%q): %v", filter, err)
	}
	return sql
}

func TestCompileStatsColumns(t *testing.T) {
	tests := []struct {
		filter  string
		headers []string
		exprs   []string
	}{
		{
			filter:  "stats count()",
			headers: []string{"count()"},
			exprs:   []string{"count(*)"},
		},
		{
			filter:  "stats count(trace_id)",
			headers: []string{"count(trace_id)"},
			exprs:   []string{`count((fields->>'$."trace_id"'))`},
		},
		{
			filter:  "stats avg(latency_ms)",
			headers: []string{"avg(latency_ms)"},
			exprs:   []string{`avg(TRY_CAST((fields->>'$."latency_ms"') AS DOUBLE))`},
		},
		{
			// A promoted field is a real column, read directly rather than
			// extracted from JSON on every row.
			filter:  "stats sum(status)",
			headers: []string{"sum(status)"},
			exprs:   []string{`sum(TRY_CAST("f_status" AS DOUBLE))`},
		},
		{
			filter:  "stats p99(latency_ms)",
			headers: []string{"p99(latency_ms)"},
			exprs:   []string{`quantile_cont(TRY_CAST((fields->>'$."latency_ms"') AS DOUBLE), 0.99)`},
		},
		{
			// Groupings first, then aggregates: the order every stats table has.
			filter:  "stats count(), p50(latency_ms) by level",
			headers: []string{"level", "count()", "p50(latency_ms)"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.filter, func(t *testing.T) {
			got := compileStats(t, tc.filter)

			if len(got.Select) != len(tc.headers) {
				t.Fatalf("got %d column(s), want %d: %+v", len(got.Select), len(tc.headers), got.Select)
			}
			for i, want := range tc.headers {
				if got.Select[i].Name != want {
					t.Errorf("column %d named %q, want %q", i, got.Select[i].Name, want)
				}
			}
			for i, want := range tc.exprs {
				if got.Select[i].Expr != want {
					t.Errorf("column %d expression =\n  %s\nwant\n  %s", i, got.Select[i].Expr, want)
				}
			}
		})
	}
}

// The bucket width and origin are numbers this package computed, so they are
// the only things formatted into the statement.
func TestCompileStatsBin(t *testing.T) {
	got := compileStats(t, "stats count() by bin(5m)")

	if got.Bin != 5*time.Minute {
		t.Errorf("bin width = %s, want 5m", got.Bin)
	}
	want := "time_bucket(INTERVAL '300000000' MICROSECOND, ts, TIMESTAMP '2026-08-13 00:00:00')"
	if got.Select[0].Expr != want {
		t.Errorf("bin expression =\n  %s\nwant\n  %s", got.Select[0].Expr, want)
	}
	if !got.Select[0].Bin || !got.Select[0].Group {
		t.Errorf("the bin column is not marked as a grouping bin: %+v", got.Select[0])
	}
	if got.Select[0].Present != "ts IS NOT NULL" {
		t.Errorf("a record with no timestamp is not excluded from the buckets: %q", got.Select[0].Present)
	}
}

// A grouping column carries the condition a record must meet to belong to a
// group, so the caller can both apply it and report what it excluded.
func TestCompileStatsGroupsCarryTheirPresenceTest(t *testing.T) {
	got := compileStats(t, "stats count() by path, region")

	for i, want := range []string{`("f_path") IS NOT NULL`, `((fields->>'$."region"')) IS NOT NULL`} {
		if got.Select[i].Present != want {
			t.Errorf("column %d presence test = %q, want %q", i, got.Select[i].Present, want)
		}
	}
	for _, col := range got.Select[2:] {
		if col.Present != "" {
			t.Errorf("aggregate %q carries a presence test %q", col.Name, col.Present)
		}
	}
}

// Ordering is by time when the grouping has a bin, and by the first aggregate
// otherwise. A rate over time read out of order is not a rate over time.
func TestCompileStatsOrdering(t *testing.T) {
	tests := []struct {
		filter  string
		groupBy string
		orderBy string
	}{
		{"stats count()", "", ""},
		{"stats count() by level", "GROUP BY 1", "ORDER BY 2 DESC NULLS LAST, 1"},
		{"stats count(), avg(latency_ms) by level", "GROUP BY 1", "ORDER BY 2 DESC NULLS LAST, 1"},
		{"stats count() by bin(1m)", "GROUP BY 1", "ORDER BY 1"},
		{"stats count() by level, bin(1m)", "GROUP BY 1, 2", "ORDER BY 2, 1"},
		{"stats count() by bin(1m), level", "GROUP BY 1, 2", "ORDER BY 1, 2"},
	}

	for _, tc := range tests {
		t.Run(tc.filter, func(t *testing.T) {
			got := compileStats(t, tc.filter)
			if got.GroupBy != tc.groupBy {
				t.Errorf("GROUP BY = %q, want %q", got.GroupBy, tc.groupBy)
			}
			if got.OrderBy != tc.orderBy {
				t.Errorf("ORDER BY = %q, want %q", got.OrderBy, tc.orderBy)
			}
		})
	}
}

// Only the fields a numeric aggregate reads are probed, once each, and count is
// not one of them: counting the records that carry a field is meaningful
// whatever it holds.
func TestCompileStatsListsNumericFields(t *testing.T) {
	got := compileStats(t, "stats count(), count(path), avg(latency_ms), p99(latency_ms), sum(status)")

	var names []string
	for _, n := range got.Numeric {
		names = append(names, n.Field)
	}
	if strings.Join(names, ",") != "latency_ms,status" {
		t.Errorf("numeric fields = %v, want [latency_ms status]", names)
	}
	if got.Numeric[0].Agg != "avg(latency_ms)" {
		t.Errorf("first aggregate reading latency_ms = %q, want avg(latency_ms)", got.Numeric[0].Agg)
	}
}

// An unknown field errors with a suggestion here exactly as it does in a
// filter, rather than producing a table of nothing.
func TestCompileStatsUnknownField(t *testing.T) {
	for _, filter := range []string{"stats avg(latancy_ms)", "stats count() by pth"} {
		q, err := Parse(filter)
		if err != nil {
			t.Fatalf("Parse(%q): %v", filter, err)
		}
		_, err = CompileStats(q.Stats, statsSchema(), StatsOptions{})

		var unknown *UnknownFieldError
		if !asUnknownField(err, &unknown) {
			t.Fatalf("CompileStats(%q) error = %v, want an UnknownFieldError", filter, err)
		}
		if len(unknown.Suggestions) == 0 {
			t.Errorf("CompileStats(%q) suggested nothing", filter)
		}
	}
}

func asUnknownField(err error, target **UnknownFieldError) bool {
	if e, ok := err.(*UnknownFieldError); ok {
		*target = e
		return true
	}
	return false
}

// A heading is the DSL text of the column — the words that would ask the
// question again — and it reaches SQL through the same identifier quoter the
// schema uses, so an awkward field name cannot break the statement.
func TestStatsColumnAliasIsQuoted(t *testing.T) {
	schema := statsSchema()
	schema.Fields = append(schema.Fields, `a"b`)

	q, err := Parse(`stats count() by "a\"b"`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, err := CompileStats(q.Stats, schema, StatsOptions{})
	if err != nil {
		t.Fatalf("CompileStats: %v", err)
	}

	// The heading keeps the DSL quoting that makes the name unambiguous, and
	// the identifier quoter doubles the quotes around it.
	if want := `AS """a\""b"""`; !strings.HasSuffix(got.Select[0].SelectItem(), want) {
		t.Errorf("alias = %s, want it to end %s", got.Select[0].SelectItem(), want)
	}
}

// Nothing in a stats clause is a value, so nothing in it needs a parameter.
// The invariant is worth asserting: a placeholder here would be one the caller
// has no argument for.
func TestCompileStatsHasNoPlaceholders(t *testing.T) {
	for _, filter := range []string{
		"stats count(), avg(latency_ms), p99(status) by path, region, bin(1m)",
		`stats sum("odd key") by "a b"`,
	} {
		q, err := Parse(filter)
		if err != nil {
			t.Fatalf("Parse(%q): %v", filter, err)
		}
		schema := statsSchema()
		schema.Fields = append(schema.Fields, "odd key", "a b")

		got, err := CompileStats(q.Stats, schema, StatsOptions{})
		if err != nil {
			t.Fatalf("CompileStats(%q): %v", filter, err)
		}
		for _, col := range got.Select {
			if strings.Contains(col.Expr, "?") {
				t.Errorf("column %q compiled a placeholder: %s", col.Name, col.Expr)
			}
		}
	}
}
