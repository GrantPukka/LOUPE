package query

import (
	"strings"
	"testing"
)

func testSchema() Schema {
	return Schema{
		Fields:  []string{"status", "latency_ms", "trace_id", "user_id", "region", "path"},
		Sources: []string{"checkout-api", "auth-svc", "nginx", "postgres", "payment-worker"},
	}
}

func compile(t *testing.T, input string) SQL {
	t.Helper()
	q := mustParse(t, input)
	sql, err := Compile(q, testSchema())
	if err != nil {
		t.Fatalf("Compile(%q): %v", input, err)
	}
	return sql
}

// Every value from the query string must arrive as a parameter. String
// concatenation is how injection bugs and unfixable precedence problems get in,
// and the spec forbids it outright.
func TestValuesAreAlwaysParameterised(t *testing.T) {
	tests := []struct {
		input     string
		mustNotBe []string
		wantArgs  []any
	}{
		{"level:error", []string{"'error'"}, []any{"error"}},
		{"trace_id:a91c40f2", []string{"a91c40f2"}, []any{"a91c40f2"}},
		{"status:>=500", []string{"500"}, []any{float64(500)}},
		{"region:eu-west-1", []string{"eu-west-1"}, []any{"eu-west-1"}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			sql := compile(t, tt.input)

			for _, forbidden := range tt.mustNotBe {
				if strings.Contains(sql.Where, forbidden) {
					t.Errorf("value %q was interpolated into the SQL:\n  %s", forbidden, sql.Where)
				}
			}
			if len(sql.Args) != len(tt.wantArgs) {
				t.Fatalf("got %d args, want %d: %#v", len(sql.Args), len(tt.wantArgs), sql.Args)
			}
			for i, want := range tt.wantArgs {
				if sql.Args[i] != want {
					t.Errorf("arg %d = %#v, want %#v", i, sql.Args[i], want)
				}
			}
		})
	}
}

// A quote in a value must never be able to break out of the statement.
func TestInjectionAttemptStaysAParameter(t *testing.T) {
	sql := compile(t, `user_id:"'; DROP TABLE logs; --"`)

	if strings.Contains(sql.Where, "DROP") {
		t.Fatalf("input reached the SQL text:\n  %s", sql.Where)
	}
	if len(sql.Args) != 1 || sql.Args[0] != `'; DROP TABLE logs; --` {
		t.Errorf("args = %#v, want the whole string as one parameter", sql.Args)
	}
}

// level:>=warn expands to a list rather than a rank lookup. That keeps the
// predicate simple and makes the unranked case correct for free.
func TestLevelComparisonExpandsToTheRightSet(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"level:error", []string{"error"}},
		{"level:error,fatal", []string{"error", "fatal"}},
		{"level:>=warn", []string{"warn", "error", "fatal"}},
		{"level:>warn", []string{"error", "fatal"}},
		{"level:<=info", []string{"trace", "debug", "info"}},
		{"level:<info", []string{"trace", "debug"}},
		{"level:>=trace", []string{"trace", "debug", "info", "warn", "error", "fatal"}},
		// Aliases normalise before expanding.
		{"level:>=WARNING", []string{"warn", "error", "fatal"}},
		{"level:W", []string{"warn"}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			sql := compile(t, tt.input)

			got := make([]string, len(sql.Args))
			for i, a := range sql.Args {
				got[i], _ = a.(string)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("levels = %v, want %v", got, tt.want)
			}
		})
	}
}

// A level outside the canonical set matches only on exact equality, so a
// custom "audit" level must not be swept up by level:>=warn.
func TestUnrankedLevelIsNotSweptUpByComparison(t *testing.T) {
	sql := compile(t, "level:>=warn")
	for _, a := range sql.Args {
		if a == "audit" {
			t.Fatal("an unranked level appeared in a comparison expansion")
		}
	}

	// It is still matchable exactly.
	exact := compile(t, "level:audit")
	if len(exact.Args) != 1 || exact.Args[0] != "audit" {
		t.Errorf("level:audit args = %#v", exact.Args)
	}

	// And comparing against it is an error with a usable message rather than
	// a silent empty result.
	q := mustParse(t, "level:>=audit")
	_, err := Compile(q, testSchema())
	if err == nil {
		t.Fatal("level:>=audit compiled; an unrankable comparison should error")
	}
	if !strings.Contains(err.Error(), "trace, debug, info") {
		t.Errorf("error does not list the valid levels: %v", err)
	}
}

// The trap: NOT (level = 'debug') is NULL for a record with no level, and a
// NULL predicate does not match. The obvious compilation silently hides every
// record that has no level at all.
func TestNegationDoesNotHideNullRecords(t *testing.T) {
	for _, input := range []string{"-level:debug", "-source:nginx", "-healthz", "-message~healthz"} {
		t.Run(input, func(t *testing.T) {
			sql := compile(t, input)
			if !strings.Contains(sql.Where, "coalesce") {
				t.Errorf("negation is not NULL-safe; records with a NULL value "+
					"would be silently excluded:\n  %s", sql.Where)
			}
		})
	}
}

func TestExistenceTerms(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"trace_id:*", "IS NOT NULL"},
		{"trace_id:none", "IS NULL"},
		{"ts:none", "IS NULL"},
		{"level:none", "IS NULL"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			sql := compile(t, tt.input)
			if !strings.Contains(sql.Where, tt.want) {
				t.Errorf("Where = %q, want it to contain %q", sql.Where, tt.want)
			}
			if len(sql.Args) != 0 {
				t.Errorf("existence tests need no parameters, got %#v", sql.Args)
			}
		})
	}
}

// Smart case: case-insensitive unless the pattern contains an uppercase
// character, which is ripgrep's behaviour.
func TestSmartCase(t *testing.T) {
	lower := compile(t, "message~timeout")
	if !strings.Contains(lower.Where, "lower(") {
		t.Errorf("an all-lowercase pattern should match case-insensitively:\n  %s", lower.Where)
	}

	upper := compile(t, "message~Timeout")
	if strings.Contains(upper.Where, "lower(") {
		t.Errorf("a pattern with an uppercase character should be case-sensitive:\n  %s", upper.Where)
	}
}

func TestRegexGetsSmartCaseFlag(t *testing.T) {
	lower := compile(t, `message~/^get /`)
	if len(lower.Args) != 1 || !strings.HasPrefix(lower.Args[0].(string), "(?i)") {
		t.Errorf("lowercase regex should be case-insensitive: %#v", lower.Args)
	}

	upper := compile(t, `message~/^GET /`)
	if strings.HasPrefix(upper.Args[0].(string), "(?i)") {
		t.Errorf("regex with uppercase should stay case-sensitive: %#v", upper.Args)
	}
}

// Searching for 100% must find the text, not match everything.
func TestLikeWildcardsInValuesAreEscaped(t *testing.T) {
	sql := compile(t, `message~"100%"`)

	arg, _ := sql.Args[0].(string)
	if !strings.Contains(arg, `\%`) {
		t.Errorf("percent sign not escaped in a LIKE pattern: %q", arg)
	}
}

// source:check finds checkout-api. Ambiguity is an error listing candidates,
// never a silent pick.
func TestSourcePrefixMatching(t *testing.T) {
	sql := compile(t, "source:check")
	if len(sql.Args) != 1 || sql.Args[0] != "checkout-api" {
		t.Errorf("source:check did not expand to checkout-api: %#v", sql.Args)
	}

	exact := compile(t, "source:nginx")
	if exact.Args[0] != "nginx" {
		t.Errorf("exact source match changed: %#v", exact.Args)
	}

	q := mustParse(t, "source:p")
	_, err := Compile(q, testSchema())
	if err == nil {
		t.Fatal("an ambiguous prefix should error")
	}
	var ambiguous *AmbiguousSourceError
	if !asError(err, &ambiguous) {
		t.Fatalf("error type = %T, want AmbiguousSourceError", err)
	}
	if len(ambiguous.Candidates) != 2 {
		t.Errorf("candidates = %v, want postgres and payment-worker", ambiguous.Candidates)
	}
	if !strings.Contains(err.Error(), "postgres") || !strings.Contains(err.Error(), "payment-worker") {
		t.Errorf("error does not list the candidates: %v", err)
	}
}

// A source that is genuinely not in this data is a meaningful empty result, not
// an error — unlike a typo'd field name.
func TestUnknownSourceIsNotAnError(t *testing.T) {
	sql := compile(t, "source:not-here")
	if sql.Args[0] != "not-here" {
		t.Errorf("args = %#v", sql.Args)
	}
}

// file: matches the base name as well as the full path, and supports globs so
// that a rotation group can be selected in one term.
func TestFileMatching(t *testing.T) {
	exact := compile(t, "file:access.log")
	if !strings.Contains(exact.Where, "file =") || !strings.Contains(exact.Where, "LIKE") {
		t.Errorf("file: should match both the full path and the base name:\n  %s", exact.Where)
	}

	glob := compile(t, "file:access.log*")
	if !strings.Contains(glob.Where, "GLOB") {
		t.Errorf("a value with a wildcard should compile to GLOB:\n  %s", glob.Where)
	}
}

// The most common failure this DSL has to prevent: a typo'd field name
// returning zero rows and being believed.
func TestUnknownFieldErrorsWithASuggestion(t *testing.T) {
	schema := Schema{Fields: []string{"severity", "service", "status"}}

	q := mustParse(t, "sevrity:error")
	_, err := Compile(q, schema)
	if err == nil {
		t.Fatal("unknown field compiled; it must error rather than match nothing")
	}

	var unknown *UnknownFieldError
	if !asError(err, &unknown) {
		t.Fatalf("error type = %T, want UnknownFieldError", err)
	}

	if len(unknown.Suggestions) == 0 {
		t.Fatal("no suggestions offered")
	}
	if unknown.Suggestions[0] != "severity" {
		t.Errorf("best suggestion = %q, want severity", unknown.Suggestions[0])
	}

	msg := err.Error()
	for _, want := range []string{`unknown field "sevrity"`, "did you mean", "severity", "fields present"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q:\n%s", want, msg)
		}
	}
}

func TestKnownFieldsResolve(t *testing.T) {
	for _, input := range []string{
		"level:error", "message~x", "source:nginx", "file:a.log", "format:jsonl",
		"status:500", "latency_ms:>10", "trace_id:abc", "raw~x", "line_no:>5",
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := Compile(mustParse(t, input), testSchema()); err != nil {
				t.Errorf("Compile(%q) failed: %v", input, err)
			}
		})
	}
}

// Field names are matched case-insensitively, so Status finds status.
func TestFieldLookupIsCaseInsensitive(t *testing.T) {
	if _, err := Compile(mustParse(t, "Status:500"), testSchema()); err != nil {
		t.Errorf("Status should resolve to status: %v", err)
	}
	if _, err := Compile(mustParse(t, "LEVEL:error"), testSchema()); err != nil {
		t.Errorf("LEVEL should resolve to level: %v", err)
	}
}

// Numeric comparison, so that 9 does not sort above 10.
func TestNumericComparisonCasts(t *testing.T) {
	numeric := compile(t, "latency_ms:>1000")
	if !strings.Contains(numeric.Where, "TRY_CAST") {
		t.Errorf("numeric comparison should cast:\n  %s", numeric.Where)
	}
	if numeric.Args[0] != float64(1000) {
		t.Errorf("arg = %#v, want a float", numeric.Args[0])
	}

	// A non-numeric value compares as text.
	textual := compile(t, "region:>eu")
	if strings.Contains(textual.Where, "TRY_CAST") {
		t.Errorf("a non-numeric comparison should not cast:\n  %s", textual.Where)
	}
}

// Free text searches the message, the fields, and the raw line. Raw matters
// because a record no parser understood is exactly what someone is hunting for.
func TestFreeTextSearchesMessageFieldsAndRaw(t *testing.T) {
	sql := compile(t, "timeout")

	for _, want := range []string{"message", "fields", "raw"} {
		if !strings.Contains(sql.Where, want) {
			t.Errorf("free text does not search %s:\n  %s", want, sql.Where)
		}
	}
}

func TestEmptyQueryMatchesEverything(t *testing.T) {
	sql := compile(t, "")
	if sql.Where != "TRUE" {
		t.Errorf("Where = %q, want TRUE", sql.Where)
	}
}

func TestTermsAreANDed(t *testing.T) {
	sql := compile(t, "level:error source:nginx")
	if !strings.Contains(sql.Where, " AND ") {
		t.Errorf("terms should be ANDed:\n  %s", sql.Where)
	}
}

func TestValueListIsORed(t *testing.T) {
	sql := compile(t, "trace_id:a,b")
	if !strings.Contains(sql.Where, " OR ") {
		t.Errorf("a comma list should compile to OR:\n  %s", sql.Where)
	}
}

// A time term reaching the compiler unresolved is a programming error, and it
// must say so clearly rather than being silently dropped — dropping it would
// widen the query without telling anyone.
func TestUnresolvedTimeTermErrors(t *testing.T) {
	q := mustParse(t, "last:15m")
	_, err := Compile(q, testSchema())
	if err == nil {
		t.Fatal("an unresolved time term compiled")
	}

	var unresolved *UnresolvedTimeError
	if !asError(err, &unresolved) {
		t.Fatalf("error type = %T, want UnresolvedTimeError", err)
	}
	if !strings.Contains(err.Error(), "last:15m") {
		t.Errorf("error should name the term: %v", err)
	}
}

// asError is errors.As without the import, kept local so the test file states
// its own expectations.
func asError[T error](err error, target *T) bool {
	if v, ok := err.(T); ok {
		*target = v
		return true
	}
	return false
}
