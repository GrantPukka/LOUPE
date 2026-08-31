package query

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseStats(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  *Stats
		terms int
	}{
		{
			name:  "count over everything",
			input: "stats count()",
			want:  &Stats{Aggs: []Aggregate{{Func: AggCount}}},
		},
		{
			name:  "count(*) means the same as count()",
			input: "stats count(*)",
			want:  &Stats{Aggs: []Aggregate{{Func: AggCount}}},
		},
		{
			name:  "count of a field counts the records that carry it",
			input: "stats count(trace_id)",
			want:  &Stats{Aggs: []Aggregate{{Func: AggCount, Field: "trace_id"}}},
		},
		{
			name:  "count_distinct reads a field",
			input: "stats count_distinct(client)",
			want:  &Stats{Aggs: []Aggregate{{Func: AggCountDistinct, Field: "client"}}},
		},
		{
			name:  "the headline shape",
			input: "stats count() by level",
			want: &Stats{
				Aggs: []Aggregate{{Func: AggCount}},
				By:   []GroupKey{{Field: "level"}},
			},
		},
		{
			name:  "a percentile of a field",
			input: "stats p99(latency_ms) by path",
			want: &Stats{
				Aggs: []Aggregate{{Func: AggP99, Field: "latency_ms"}},
				By:   []GroupKey{{Field: "path"}},
			},
		},
		{
			name:  "several aggregates and several groupings",
			input: "stats count(), avg(latency_ms), max(bytes) by level, bin(1m)",
			want: &Stats{
				Aggs: []Aggregate{
					{Func: AggCount},
					{Func: AggAvg, Field: "latency_ms"},
					{Func: AggMax, Field: "bytes"},
				},
				By: []GroupKey{{Field: "level"}, {Bin: time.Minute}},
			},
		},
		{
			name:  "case is not significant in the vocabulary",
			input: "STATS P95(latency_ms) BY BIN(30S)",
			want: &Stats{
				Aggs: []Aggregate{{Func: AggP95, Field: "latency_ms"}},
				By:   []GroupKey{{Bin: 30 * time.Second}},
			},
		},
		{
			name:  "a field name that needs quoting",
			input: `stats sum("odd key") by "a b"`,
			want: &Stats{
				Aggs: []Aggregate{{Func: AggSum, Field: "odd key"}},
				By:   []GroupKey{{Field: "a b"}},
			},
		},
		{
			name:  "a field genuinely called by",
			input: "stats count() by by",
			want: &Stats{
				Aggs: []Aggregate{{Func: AggCount}},
				By:   []GroupKey{{Field: "by"}},
			},
		},
		{
			name:  "the clause sits alongside ordinary terms",
			input: "level:>=error source:nginx stats count() by path",
			want: &Stats{
				Aggs: []Aggregate{{Func: AggCount}},
				By:   []GroupKey{{Field: "path"}},
			},
			terms: 2,
		},
		{
			name:  "terms may follow the clause",
			input: "stats count() by path level:error",
			want: &Stats{
				Aggs: []Aggregate{{Func: AggCount}},
				By:   []GroupKey{{Field: "path"}},
			},
			terms: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.input)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.input, err)
			}
			if !reflect.DeepEqual(got.Stats, tc.want) {
				t.Errorf("Parse(%q).Stats =\n  %+v\nwant\n  %+v", tc.input, got.Stats, tc.want)
			}
			if len(got.Terms) != tc.terms {
				t.Errorf("Parse(%q) kept %d term(s), want %d", tc.input, len(got.Terms), tc.terms)
			}
		})
	}
}

// A field called stats is still reachable: the keyword only counts where a term
// begins and is not being used as a name.
func TestStatsIsOnlyAKeywordWhereATermBegins(t *testing.T) {
	for _, input := range []string{"stats:5", "stats~high", `"stats":5`} {
		q, err := Parse(input)
		if err != nil {
			t.Fatalf("Parse(%q): %v", input, err)
		}
		if q.Stats != nil {
			t.Errorf("Parse(%q) read a stats clause, want a field term", input)
		}
		if len(q.FieldTerms()) != 1 {
			t.Errorf("Parse(%q) produced %d field term(s), want 1", input, len(q.Terms))
		}
	}
}

// The cost of reserving the word: a free-text search for it has to be quoted,
// and the parser marks it so that rendering puts the quotes back.
func TestFreeTextStatsRoundTrips(t *testing.T) {
	q, err := Parse(`"stats"`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := q.String(); got != `"stats"` {
		t.Errorf("rendered %q, want %q", got, `"stats"`)
	}

	// Bare, it is a clause with nothing in it, and the error says how to search
	// for the word instead.
	_, err = Parse("stats")
	if err == nil {
		t.Fatal("bare stats parsed, want an error")
	}
	if !strings.Contains(err.Error(), `"stats"`) {
		t.Errorf("error does not say how to search for the word:\n%v", err)
	}
}

func TestParseStatsErrors(t *testing.T) {
	tests := []struct {
		input string
		says  string
	}{
		{"stats", "at least one aggregate"},
		{"stats by level", "before by"},
		{"stats foo", "not an aggregate"},
		{"stats mean(latency_ms)", "unknown aggregate"},
		{"stats avg()", "needs a field"},
		{"stats p99()", "needs a field"},
		{"stats count(latency_ms", "not a complete aggregate"},
		{"stats count() by", "expected a field after by"},
		{"stats count() by sum(x)", "cannot be grouped by"},
		{"stats count() by bin()", "not a bucket width"},
		{"stats count() by bin(x)", "not a bucket width"},
		{"stats count() by bin(0s)", "no width"},
		{"stats count() by bin(0.5s)", "finer than a second"},
		{"stats count() stats count()", "only have one stats clause"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			_, err := Parse(tc.input)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want an error", tc.input)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("Parse(%q) error does not mention %q:\n%v", tc.input, tc.says, err)
			}
		})
	}
}

// An unknown aggregate suggests, exactly as an unknown field does. Returning
// zeroes for a misspelt function is the failure this rule exists to prevent.
func TestUnknownAggregateSuggests(t *testing.T) {
	_, err := Parse("stats mim(latency_ms)")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "min()") {
		t.Errorf("error does not suggest min():\n%v", err)
	}
}

// docs/FILTER-DSL.md requires a round-trip test per term type, and an
// aggregation is no different: the UI writes rendered filters back into the box.
func TestStatsRoundTrip(t *testing.T) {
	inputs := []string{
		"stats count()",
		"stats count(*)",
		"stats count(trace_id)",
		"stats sum(bytes)",
		"stats avg(latency_ms)",
		"stats min(latency_ms)",
		"stats max(latency_ms)",
		"stats p50(latency_ms)",
		"stats p95(latency_ms)",
		"stats p99(latency_ms)",
		"stats count() by level",
		"stats count() by bin(1s)",
		"stats count() by bin(30s)",
		"stats count() by bin(1m)",
		"stats count() by bin(1h)",
		"stats count() by bin(1d)",
		"stats count() by bin(1w)",
		"stats count() by bin(60s)",
		"stats count(), p99(latency_ms) by level, path, bin(5m)",
		"level:>=error -source:nginx timeout stats count() by path",
		`stats p99("odd key") by "a b", "last", "-x", "bin(1m)"`,
		`"stats" stats count()`,
	}

	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			first, err := Parse(input)
			if err != nil {
				t.Fatalf("Parse(%q): %v", input, err)
			}

			rendered := first.String()
			second, err := Parse(rendered)
			if err != nil {
				t.Fatalf("rendered %q does not parse: %v", rendered, err)
			}
			if !reflect.DeepEqual(first, second) {
				t.Errorf("round trip changed the query\n  input:    %q\n  rendered: %q", input, rendered)
			}
			if again := second.String(); again != rendered {
				t.Errorf("rendering is not stable: %q then %q", rendered, again)
			}
		})
	}
}

// A bin renders in the largest unit that divides it, which is what makes
// bin(60s) and bin(1m) the same query rather than two spellings that drift.
func TestBinRendersInWholeUnits(t *testing.T) {
	tests := map[time.Duration]string{
		time.Second:            "1s",
		90 * time.Second:       "90s",
		time.Minute:            "1m",
		15 * time.Minute:       "15m",
		time.Hour:              "1h",
		24 * time.Hour:         "1d",
		7 * 24 * time.Hour:     "1w",
		36 * time.Hour:         "36h",
		2 * 7 * 24 * time.Hour: "2w",
	}

	for width, want := range tests {
		if got := (GroupKey{Bin: width}).String(); got != "bin("+want+")" {
			t.Errorf("bin of %s rendered %q, want bin(%s)", width, got, want)
		}
	}
}
