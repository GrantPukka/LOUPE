package schema

import (
	"strings"
	"testing"
)

// one builds a sample from a single source, for the cases where the source
// does not matter.
func one(fields ...map[string]any) []Sample {
	out := make([]Sample, len(fields))
	for i, f := range fields {
		out[i] = Sample{Source: "app", Fields: f}
	}
	return out
}

// repeat produces n identical samples from one source.
func repeat(source string, n int, fields map[string]any) []Sample {
	out := make([]Sample, n)
	for i := range out {
		out[i] = Sample{Source: source, Fields: fields}
	}
	return out
}

func promotionFor(promotions []Promotion, field string) (Promotion, bool) {
	for _, p := range promotions {
		if p.Field == field {
			return p, true
		}
	}
	return Promotion{}, false
}

func skipReason(skips []Skip, field string) string {
	for _, s := range skips {
		if s.Field == field {
			return s.Reason
		}
	}
	return ""
}

func TestCoverageThreshold(t *testing.T) {
	var samples []Sample
	for i := 0; i < 10; i++ {
		fields := map[string]any{"always": int64(1)}
		if i < 7 {
			fields["often"] = int64(1)
		}
		if i < 3 {
			fields["rarely"] = int64(1)
		}
		samples = append(samples, Sample{Source: "app", Fields: fields})
	}

	promotions, skips := Infer(samples, Options{})

	for _, want := range []string{"always", "often"} {
		if _, ok := promotionFor(promotions, want); !ok {
			t.Errorf("%q was not promoted", want)
		}
	}
	if _, ok := promotionFor(promotions, "rarely"); ok {
		t.Error("a field in 30% of records was promoted")
	}
	if reason := skipReason(skips, "rarely"); !strings.Contains(reason, "threshold") {
		t.Errorf("skip reason does not explain the threshold: %q", reason)
	}
}

// The threshold is measured within a source, not across the directory.
//
// A field carried by every Nginx record is a good column even though Postgres
// never sets it. Judged globally it would fall below 60% in any directory with
// more than two formats, and almost nothing would ever be promoted.
func TestCoverageIsJudgedPerSource(t *testing.T) {
	var samples []Sample
	samples = append(samples, repeat("nginx", 100, map[string]any{"status": int64(200)})...)
	samples = append(samples, repeat("postgres", 100, map[string]any{"pid": int64(20044)})...)
	samples = append(samples, repeat("syslog", 100, map[string]any{"facility": int64(1)})...)

	promotions, _ := Infer(samples, Options{})

	// Each field is in exactly a third of all records, but all of its own
	// source's records.
	for _, want := range []string{"status", "pid", "facility"} {
		p, ok := promotionFor(promotions, want)
		if !ok {
			t.Fatalf("%q was not promoted; per-source coverage was not used", want)
		}
		if p.BestCoverage != 1.0 {
			t.Errorf("%q BestCoverage = %.2f, want 1.0", want, p.BestCoverage)
		}
		if p.Coverage > 0.4 {
			t.Errorf("%q Coverage = %.2f; the global figure should still be recorded", want, p.Coverage)
		}
	}

	if p, _ := promotionFor(promotions, "status"); p.BestSource != "nginx" {
		t.Errorf("BestSource = %q, want nginx", p.BestSource)
	}
}

func TestTypeInference(t *testing.T) {
	tests := []struct {
		name   string
		values []any
		want   Kind
	}{
		{"integers", []any{int64(1), int64(2), int64(3)}, KindInt},
		{"floats", []any{1.5, 2.5}, KindFloat},
		{"integers and floats widen to float", []any{int64(1), 2.5}, KindFloat},
		{"booleans", []any{true, false}, KindBool},
		{"strings", []any{"a", "b"}, KindString},
		{"rfc3339 timestamps", []any{"2026-08-13T14:00:00Z", "2026-08-13T15:00:00Z"}, KindTimestamp},
		{"dates", []any{"2026-08-13", "2026-08-14"}, KindTimestamp},

		// The trap ARCHITECTURE names: a field whose type changes halfway
		// through. Widening loses nothing; narrowing turns half the values
		// into NULL.
		{"type changes mid-file", []any{int64(1), int64(2), "n/a"}, KindString},
		{"booleans then strings", []any{true, "maybe"}, KindString},
		{"timestamps then numbers", []any{"2026-08-13T14:00:00Z", int64(5)}, KindString},

		// A numeric-looking string stays a string. The parsers already decided
		// this — logfmt deliberately keeps 007 a string, because an id with a
		// leading zero is not the number seven.
		{"numeric-looking strings", []any{"007", "008"}, KindString},

		// A bare integer is far more often an id, a port, or a byte count than
		// epoch millis. Turning one into a date would be a confident wrong
		// answer.
		{"large integers are not timestamps", []any{int64(1755091200000), int64(1755091300000)}, KindInt},

		// Nulls are not evidence of a type either way.
		{"nulls among integers", []any{int64(1), nil, int64(3)}, KindInt},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var samples []Sample
			for _, v := range tt.values {
				samples = append(samples, Sample{Source: "app", Fields: map[string]any{"f": v}})
			}

			promotions, skips := Infer(samples, Options{})
			p, ok := promotionFor(promotions, "f")
			if !ok {
				t.Fatalf("field not promoted: %s", skipReason(skips, "f"))
			}
			if p.Kind != tt.want {
				t.Errorf("Kind = %s, want %s", p.Kind, tt.want)
			}
		})
	}
}

func TestNestedValuesAreNotPromoted(t *testing.T) {
	samples := one(
		map[string]any{"http": map[string]any{"status": int64(200)}},
		map[string]any{"http": map[string]any{"status": int64(500)}},
		map[string]any{"tags": []any{"a", "b"}},
		map[string]any{"tags": []any{"c"}},
	)

	promotions, skips := Infer(samples, Options{})

	for _, field := range []string{"http", "tags"} {
		if _, ok := promotionFor(promotions, field); ok {
			t.Errorf("%q holds a nested value and should not be a column", field)
		}
		if reason := skipReason(skips, field); reason == "" {
			t.Errorf("%q skipped without a reason", field)
		}
	}
}

// A field named level or ts must not shadow the built-in column of that name.
func TestReservedNamesAreSkipped(t *testing.T) {
	samples := one(
		map[string]any{"level": "custom", "ts": "x", "raw": "y", "ok": int64(1)},
		map[string]any{"level": "custom", "ts": "x", "raw": "y", "ok": int64(1)},
	)

	promotions, skips := Infer(samples, Options{})

	for _, field := range []string{"level", "ts", "raw"} {
		if _, ok := promotionFor(promotions, field); ok {
			t.Errorf("%q collides with a built-in column and should be skipped", field)
		}
		if reason := skipReason(skips, field); !strings.Contains(reason, "built-in") {
			t.Errorf("%q skip reason = %q", field, reason)
		}
	}
	if _, ok := promotionFor(promotions, "ok"); !ok {
		t.Error("an ordinary field was skipped alongside the reserved ones")
	}
}

func TestColumnNames(t *testing.T) {
	tests := map[string]string{
		"status":       "status",
		"latency_ms":   "latency_ms",
		"http.status":  "http_status",
		"trace-id":     "trace_id",
		"Some Field":   "some_field",
		"UPPER":        "upper",
		"a/b":          "a_b",
		"123":          "f_123",
		"!!!":          "",
		"_leading":     "leading",
		"exampleSDID@": "examplesdid",
	}

	for field, want := range tests {
		t.Run(field, func(t *testing.T) {
			if got := columnName(field); got != want {
				t.Errorf("columnName(%q) = %q, want %q", field, got, want)
			}
		})
	}
}

// Two field names that sanitise to the same column cannot both be promoted.
func TestColumnNameCollisionIsSkipped(t *testing.T) {
	samples := one(
		map[string]any{"trace-id": "a", "trace.id": "b"},
		map[string]any{"trace-id": "a", "trace.id": "b"},
	)

	promotions, skips := Infer(samples, Options{})

	if len(promotions) != 1 {
		t.Fatalf("got %d promotions, want 1 — both names sanitise to trace_id", len(promotions))
	}
	if len(skips) == 0 {
		t.Error("the losing field was dropped without a reason")
	}
}

func TestMaxColumnsCaps(t *testing.T) {
	fields := map[string]any{}
	for i := 0; i < 50; i++ {
		fields[string(rune('a'+i%26))+string(rune('a'+i/26))] = int64(i)
	}

	promotions, skips := Infer(one(fields, fields), Options{MaxColumns: 5})

	if len(promotions) != 5 {
		t.Errorf("got %d promotions, want the cap of 5", len(promotions))
	}
	if len(skips) == 0 {
		t.Error("fields below the cut were dropped without a reason")
	}
}

// The order of promotions decides which survive the MaxColumns cut, so it must
// not depend on map iteration order.
func TestPromotionOrderIsDeterministic(t *testing.T) {
	fields := map[string]any{
		"alpha": int64(1), "beta": int64(2), "gamma": int64(3),
		"delta": int64(4), "epsilon": int64(5),
	}

	first, _ := Infer(one(fields, fields), Options{})
	for i := 0; i < 20; i++ {
		got, _ := Infer(one(fields, fields), Options{})
		if len(got) != len(first) {
			t.Fatalf("run %d produced %d promotions, first produced %d", i, len(got), len(first))
		}
		for j := range got {
			if got[j].Field != first[j].Field {
				t.Fatalf("run %d ordered %q at %d, first had %q", i, got[j].Field, j, first[j].Field)
			}
		}
	}
}

func TestEmptySample(t *testing.T) {
	promotions, skips := Infer(nil, Options{})
	if promotions != nil || skips != nil {
		t.Errorf("an empty sample produced %v / %v", promotions, skips)
	}
}

func TestSQLTypes(t *testing.T) {
	tests := map[Kind]string{
		KindString:    "VARCHAR",
		KindInt:       "BIGINT",
		KindFloat:     "DOUBLE",
		KindBool:      "BOOLEAN",
		KindTimestamp: "TIMESTAMP",
	}
	for kind, want := range tests {
		if got := kind.SQLType(); got != want {
			t.Errorf("%d.SQLType() = %q, want %q", kind, got, want)
		}
	}
}
