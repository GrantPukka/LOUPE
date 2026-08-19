package query

import (
	"reflect"
	"strings"
	"testing"
)

// The filter language is the other half of the untrusted surface.
//
// A parser is fed bytes by whoever wrote the log; the DSL is fed text by
// whoever is typing at four in the morning. Neither may panic, and the round
// trip matters beyond tidiness: the UI's timeline drag writes a rendered AST
// into the filter box, so a query that does not survive render-and-reparse is
// one the user cannot re-run or share.

// seedFilters are the shapes the language actually has, so the corpus starts
// on real syntax rather than on random punctuation.
func seedFilters(f *testing.F) {
	f.Helper()

	for _, seed := range []string{
		"",
		" ",
		"level:error",
		"level:error,fatal",
		"level:>=warn",
		"-level:debug",
		"status:>=500 latency_ms:>1000",
		"message~timeout",
		`message~/^GET \/api/`,
		`"read timed out"`,
		"timeout",
		"-healthz",
		"field:*",
		"ts:none",
		"last:15m",
		"after:14:00",
		"between:14:00-15:00",
		"on:2026-08-13",
		"14:00-15:00",
		"source:nginx",
		"file:access.log*",
		"pattern:72537a34170e",
		"stats count()",
		"stats count(*)",
		"stats count(), p99(latency_ms) by path",
		"stats avg(latency_ms) by level, bin(1m)",
		"level:>=error stats count() by bin(30s)",
		`stats p99("odd key") by "a b"`,
		"stats",
		"stats by",
		"stats count() by",
		"stats count(",
		"stats )(",
		"stats:5",
		`"stats"`,
		"-stats",
		"stats count() by bin(1m) stats count()",
		`user:"a name with spaces"`,
		`"weird\"key":y`,
		`"key:with:colons":y`,
		"level:error source:nginx timeout",
		// The shapes most likely to break a lexer.
		`"`,
		`""`,
		"/",
		"//",
		`/unterminated`,
		":",
		"::",
		"a:",
		":b",
		"-",
		",",
		"a:,",
		"a:b,",
		">=",
		"a:>=",
		"\x00",
		strings.Repeat("a:b ", 64),
	} {
		f.Add(seed)
	}
}

// FuzzParse checks that no input panics and that whatever parses round-trips.
//
// A filter that parses but renders to something that parses differently is the
// bug worth hunting here: the UI writes rendered ASTs back into the box, so the
// user would see their own filter silently become a different query.
func FuzzParse(f *testing.F) {
	seedFilters(f)

	f.Fuzz(func(t *testing.T, filter string) {
		first, err := Parse(filter)
		if err != nil {
			// Rejecting nonsense is the correct outcome, not a finding.
			return
		}

		rendered := first.String()

		second, err := Parse(rendered)
		if err != nil {
			t.Fatalf("a parsed filter did not survive rendering\n  input:    %q\n  rendered: %q\n  error:    %v",
				filter, rendered, err)
		}

		// The whole query, not only its terms: an aggregation clause that did
		// not survive rendering would silently become a different summary.
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("round trip changed the AST\n  input:    %q\n  rendered: %q",
				filter, rendered)
		}

		// Rendering must also be stable, or the filter box churns every time
		// the UI writes a term back into it.
		if again := second.String(); again != rendered {
			t.Fatalf("rendering is not stable\n  input: %q\n  once:  %q\n  twice: %q",
				filter, rendered, again)
		}
	})
}

// FuzzCompile drives a parsed filter all the way to SQL.
//
// The one invariant that matters more than the rest: every value from the
// query string arrives as a parameter. CLAUDE.md forbids building SQL by
// concatenation, and a fuzzer is the only reader patient enough to check that
// claim against every shape of input rather than the handful in a table test.
func FuzzCompile(f *testing.F) {
	seedFilters(f)

	schema := Schema{
		Fields:  []string{"status", "latency_ms", "trace_id", "user_id", "region", "path"},
		Sources: []string{"checkout-api", "auth-svc", "nginx", "postgres"},
	}

	f.Fuzz(func(t *testing.T, filter string) {
		parsed, err := Parse(filter)
		if err != nil {
			return
		}

		sql, err := Compile(parsed, schema)
		if err != nil {
			// An unknown field or an unusable operator is a real answer, and
			// the error is the feature.
			return
		}

		if strings.TrimSpace(sql.Where) == "" {
			t.Fatalf("compiled %q to an empty predicate", filter)
		}

		// A placeholder per argument, and an argument per placeholder. A
		// mismatch is either a value that was interpolated into the text or a
		// parameter bound to nothing, and DuckDB binds by position.
		if got := strings.Count(sql.Where, "?"); got != len(sql.Args) {
			t.Fatalf("compiled %q with %d placeholders and %d args\n  where: %s",
				filter, got, len(sql.Args), sql.Where)
		}

		// Balanced parentheses, because the compiler builds nested predicates
		// by hand and an unbalanced one is a syntax error DuckDB reports far
		// from its cause.
		depth := 0
		for _, r := range sql.Where {
			switch r {
			case '(':
				depth++
			case ')':
				depth--
			}
			if depth < 0 {
				t.Fatalf("compiled %q to unbalanced SQL: %s", filter, sql.Where)
			}
		}
		if depth != 0 {
			t.Fatalf("compiled %q to unbalanced SQL: %s", filter, sql.Where)
		}
	})
}

// FuzzParseDuration covers the units last: and --new-since share.
func FuzzParseDuration(f *testing.F) {
	for _, seed := range []string{
		"15m", "2h", "1d", "1w", "30s", "1.5h", "0s",
		"", "m", "-1m", "1", "1x", "99999999999999999999d", "1e9s",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		d, err := ParseDuration(s)
		if err != nil {
			return
		}
		// A negative window would silently invert every range built from it.
		if d < 0 {
			t.Fatalf("ParseDuration(%q) returned %s", s, d)
		}
	})
}
