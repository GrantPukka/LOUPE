package query

import (
	"reflect"
	"strings"
	"testing"
)

func mustParse(t *testing.T, input string) Query {
	t.Helper()
	q, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse(%q): %v", input, err)
	}
	return q
}

func TestParseTerms(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []Term
	}{
		{
			name:  "empty",
			input: "",
			want:  nil,
		},
		{
			name:  "simple field",
			input: "level:error",
			want:  []Term{&FieldTerm{Key: "level", Values: []Value{{Text: "error"}}}},
		},
		{
			name:  "comparison",
			input: "status:>=500",
			want:  []Term{&FieldTerm{Key: "status", Op: OpGE, Values: []Value{{Text: "500"}}}},
		},
		{
			name:  "value list is OR within the term",
			input: "level:error,fatal",
			want: []Term{&FieldTerm{Key: "level", Values: []Value{
				{Text: "error"}, {Text: "fatal"},
			}}},
		},
		{
			name:  "negation",
			input: "-level:debug",
			want:  []Term{&FieldTerm{Key: "level", Values: []Value{{Text: "debug"}}, Negate: true}},
		},
		{
			name:  "bare word is free text",
			input: "timeout",
			want:  []Term{&FreeTerm{Value: Value{Text: "timeout"}}},
		},
		{
			name:  "quoted phrase",
			input: `"read timed out"`,
			want:  []Term{&FreeTerm{Value: Value{Text: "read timed out", Quoted: true}}},
		},
		{
			name:  "negated free text",
			input: "-healthz",
			want:  []Term{&FreeTerm{Value: Value{Text: "healthz"}, Negate: true}},
		},
		{
			name:  "substring match",
			input: "message~timeout",
			want:  []Term{&FieldTerm{Key: "message", Op: OpMatch, Values: []Value{{Text: "timeout"}}}},
		},
		{
			name:  "regex match",
			input: `message~/^GET \/api/`,
			want: []Term{&FieldTerm{Key: "message", Op: OpMatch, Values: []Value{
				{Text: "^GET /api", Regex: true},
			}}},
		},
		{
			name:  "multiple terms are ANDed",
			input: "level:error source:nginx timeout",
			want: []Term{
				&FieldTerm{Key: "level", Values: []Value{{Text: "error"}}},
				&FieldTerm{Key: "source", Values: []Value{{Text: "nginx"}}},
				&FreeTerm{Value: Value{Text: "timeout"}},
			},
		},
		{
			// A hyphen inside a value is part of the value, not a negation.
			name:  "hyphens inside values",
			input: "region:eu-west-1",
			want:  []Term{&FieldTerm{Key: "region", Values: []Value{{Text: "eu-west-1"}}}},
		},
		{
			name:  "existence and absence",
			input: "trace_id:* user_id:none",
			want: []Term{
				&FieldTerm{Key: "trace_id", Values: []Value{{Text: "*"}}},
				&FieldTerm{Key: "user_id", Values: []Value{{Text: "none"}}},
			},
		},
		{
			// Slashes are path characters outside a regex.
			name:  "path values keep their slashes",
			input: "path:/api/checkout",
			want:  []Term{&FieldTerm{Key: "path", Values: []Value{{Text: "/api/checkout"}}}},
		},
		{
			name:  "glob on file",
			input: "file:access.log*",
			want:  []Term{&FieldTerm{Key: "file", Values: []Value{{Text: "access.log*"}}}},
		},
		{
			name:  "time keyword",
			input: "last:15m",
			want:  []Term{&TimeTerm{Keyword: "last", Expr: "15m"}},
		},
		{
			name:  "time with reassembled colons",
			input: "after:14:00:00",
			want:  []Term{&TimeTerm{Keyword: "after", Expr: "14:00:00"}},
		},
		{
			name:  "bare time range needs no keyword",
			input: "14:00-15:00",
			want:  []Term{&TimeTerm{Expr: "14:00-15:00"}},
		},
		{
			name:  "ts:none is a field term, not a time term",
			input: "ts:none",
			want:  []Term{&FieldTerm{Key: "ts", Values: []Value{{Text: "none"}}}},
		},
		{
			name:  "since is an alias for after",
			input: "since:14:00",
			want:  []Term{&TimeTerm{Keyword: "since", Expr: "14:00"}},
		},
		{
			// Extra whitespace must not change the parse.
			name:  "irregular whitespace",
			input: "  level:error    timeout  ",
			want: []Term{
				&FieldTerm{Key: "level", Values: []Value{{Text: "error"}}},
				&FreeTerm{Value: Value{Text: "timeout"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mustParse(t, tt.input)
			if !reflect.DeepEqual(got.Terms, tt.want) {
				t.Errorf("Parse(%q)\n got: %s\nwant: %s", tt.input, describe(got.Terms), describe(tt.want))
			}
		})
	}
}

// The worked examples from docs/FILTER-DSL.md section 8. If the spec's own
// examples do not parse, the spec is not implemented.
func TestSpecWorkedExamples(t *testing.T) {
	examples := []struct {
		input     string
		wantTerms int
	}{
		{"14:00-15:00 level:info source:nginx", 3},
		{"between:14:00-15:00 level:>=warn -source:nginx timeout", 4},
		{"last:15m level:error status:>=500", 3},
		{"on:2026-08-13 14:11-14:14 trace_id:a91c40f2", 3},
		{"ts:none", 1},
	}

	for _, ex := range examples {
		t.Run(ex.input, func(t *testing.T) {
			got := mustParse(t, ex.input)
			if len(got.Terms) != ex.wantTerms {
				t.Errorf("got %d terms, want %d: %s", len(got.Terms), ex.wantTerms, describe(got.Terms))
			}
		})
	}
}

// parse(render(ast)) == ast, for every term type.
//
// docs/FILTER-DSL.md requires this, and it is not merely a tidiness property:
// the UI's timeline drag renders an AST into the filter box, so a term that
// does not round-trip produces a query the user cannot re-run.
func TestRoundTrip(t *testing.T) {
	inputs := []string{
		"level:error",
		"level:error,fatal",
		"level:>=warn",
		"level:<=info",
		"level:>warn",
		"level:<error",
		"-level:debug",
		"status:>=500",
		"latency_ms:>1000",
		"trace_id:a91c40f2",
		"source:nginx",
		"source:nginx,postgres",
		"-source:nginx",
		"file:access.log",
		"file:access.log*",
		"format:jsonl",
		"timeout",
		`"read timed out"`,
		"-healthz",
		"message~timeout",
		"-message~healthz",
		`message~/^GET \/api/`,
		"field:*",
		"field:none",
		"ts:none",
		"region:eu-west-1",
		"path:/api/checkout",
		"last:15m",
		"after:14:00",
		"before:15:00",
		"between:14:00-15:00",
		"on:2026-08-13",
		"after:2026-08-13T14:00:00Z",
		"14:00-15:00",
		// Template ids, which the pattern listing prints for pasting back.
		"pattern:72537a34170e",
		"pattern:72537a",
		"-pattern:72537a34170e",
		"pattern:002cf356a676,a11462dd2ea1",
		"pattern:none",
		"level:error pattern:72537a34170e",
		"level:error source:nginx timeout",
		"between:14:00-15:00 level:>=warn -source:nginx timeout",
		`user:"a name with spaces"`,
		"on:2026-08-13 14:11-14:14 trace_id:a91c40f2",
		// Quoted keys. A log file can name a field anything, so each of these
		// has to survive being rendered and re-parsed.
		`"weird\"key":y`,
		`"a key with spaces":y`,
		`"key:with:colons":y`,
		`"it's":z`,
		`-"weird\"key":y`,
		`"odd key"~timeout`,
		// Names colliding with the DSL's own vocabulary.
		`"last":15m`,
		`"on":2026-08-13`,
		`"between":x`,
		`"14":00`,
		`"-leading":y`,
	}

	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			first := mustParse(t, input)

			rendered := first.String()
			second, err := Parse(rendered)
			if err != nil {
				t.Fatalf("re-parsing rendered %q failed: %v", rendered, err)
			}

			if !reflect.DeepEqual(first.Terms, second.Terms) {
				t.Errorf("round trip changed the AST\n  input:    %q\n  rendered: %q\n  first:  %s\n  second: %s",
					input, rendered, describe(first.Terms), describe(second.Terms))
			}

			// Rendering must also be stable: render(parse(render(ast))) is the
			// same text, or the filter box would churn on every interaction.
			if again := second.String(); again != rendered {
				t.Errorf("rendering is not stable: %q then %q", rendered, again)
			}
		})
	}
}

// A quoted key names a field literally. Without that, a log file with a field
// called last or on has records that cannot be filtered on at all, because the
// keyword reading always wins.
func TestQuotedKeyNamesAFieldLiterally(t *testing.T) {
	bare := mustParse(t, "last:15m")
	if _, ok := bare.Terms[0].(*TimeTerm); !ok {
		t.Fatalf("last:15m = %T, want *TimeTerm", bare.Terms[0])
	}

	quoted := mustParse(t, `"last":15m`)
	field, ok := quoted.Terms[0].(*FieldTerm)
	if !ok {
		t.Fatalf(`"last":15m = %T, want *FieldTerm`, quoted.Terms[0])
	}
	if field.Key != "last" {
		t.Errorf("key = %q, want last", field.Key)
	}
	if len(field.Values) != 1 || field.Values[0].Text != "15m" {
		t.Errorf("values = %v, want [15m]", field.Values)
	}
}

// The keys that motivated this: a quote, a space and a colon are all legal in a
// log field name and none of them can be written bare.
func TestQuotedKeysReachOtherwiseUnreachableFields(t *testing.T) {
	tests := []struct {
		input string
		key   string
	}{
		{`"weird\"key":y`, `weird"key`},
		{`"a key with spaces":y`, "a key with spaces"},
		{`"key:with:colons":y`, "key:with:colons"},
		{`"it's":z`, "it's"},
		{`"back\\slash":z`, `back\slash`},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			q := mustParse(t, tt.input)
			field, ok := q.Terms[0].(*FieldTerm)
			if !ok {
				t.Fatalf("term = %T, want *FieldTerm", q.Terms[0])
			}
			if field.Key != tt.key {
				t.Errorf("key = %q, want %q", field.Key, tt.key)
			}
		})
	}
}

// A quoted string with no colon after it is still a phrase search, or every
// existing free-text query would change meaning.
func TestQuotedPhraseIsStillFreeText(t *testing.T) {
	q := mustParse(t, `"read timed out"`)
	if _, ok := q.Terms[0].(*FreeTerm); !ok {
		t.Fatalf(`"read timed out" = %T, want *FreeTerm`, q.Terms[0])
	}
}

func TestSyntaxErrors(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantHint string
	}{
		{"unterminated quote", `msg:"never closed`, `closing`},
		{"unterminated regex", "message~/^GET", "delimited by slashes"},
		{"dangling minus", "-", "after it"},
		{"missing value", "level:", "level:error"},
		{"missing time", "last:", "last:15m"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.input)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want an error", tt.input)
			}
			if !strings.Contains(err.Error(), tt.wantHint) {
				t.Errorf("error %q does not contain a usable hint %q", err.Error(), tt.wantHint)
			}
		})
	}
}

// A syntax error must point at the offending character, not just complain.
func TestSyntaxErrorShowsPosition(t *testing.T) {
	_, err := Parse(`level:error msg:"unterminated`)
	if err == nil {
		t.Fatal("expected an error")
	}

	msg := err.Error()
	if !strings.Contains(msg, "^") {
		t.Errorf("error does not point at the problem:\n%s", msg)
	}
	if !strings.Contains(msg, "level:error msg:") {
		t.Errorf("error does not echo the input:\n%s", msg)
	}
}

// A bare /slashed/ token is not a regex: the grammar has no bare-regex form, so
// it is an ordinary free-text search for that literal text.
func TestBareSlashesAreFreeText(t *testing.T) {
	got := mustParse(t, "/api/checkout")
	want := []Term{&FreeTerm{Value: Value{Text: "/api/checkout"}}}
	if !reflect.DeepEqual(got.Terms, want) {
		t.Errorf("got %s, want free text", describe(got.Terms))
	}
}

// Both the colon and the bare form of the match operator must parse to the
// same AST, since the spec's grammar and its examples disagree.
func TestMatchOperatorAcceptsBothForms(t *testing.T) {
	withColon := mustParse(t, "message:~timeout")
	without := mustParse(t, "message~timeout")

	if !reflect.DeepEqual(withColon.Terms, without.Terms) {
		t.Errorf("message:~timeout and message~timeout differ:\n  %s\n  %s",
			describe(withColon.Terms), describe(without.Terms))
	}
}

func describe(terms []Term) string {
	if len(terms) == 0 {
		return "(empty)"
	}
	parts := make([]string, len(terms))
	for i, t := range terms {
		parts[i] = t.String()
	}
	return strings.Join(parts, " | ")
}

// Regression: a negated term after another term was being absorbed into the
// preceding value, so `level:error -source:nginx` parsed as a field named
// "-source".
//
// Round-trip testing is structurally blind to this: the misparse renders back
// to the same text and re-parses the same wrong way. Only asserting the shape
// of the AST, or running the SQL, catches it.
func TestNegationAfterAnotherTerm(t *testing.T) {
	tests := []struct {
		input string
		want  []Term
	}{
		{
			input: "level:error -source:nginx",
			want: []Term{
				&FieldTerm{Key: "level", Values: []Value{{Text: "error"}}},
				&FieldTerm{Key: "source", Values: []Value{{Text: "nginx"}}, Negate: true},
			},
		},
		{
			input: "-source:checkout-api -source:nginx",
			want: []Term{
				&FieldTerm{Key: "source", Values: []Value{{Text: "checkout-api"}}, Negate: true},
				&FieldTerm{Key: "source", Values: []Value{{Text: "nginx"}}, Negate: true},
			},
		},
		{
			input: "timeout -healthz",
			want: []Term{
				&FreeTerm{Value: Value{Text: "timeout"}},
				&FreeTerm{Value: Value{Text: "healthz"}, Negate: true},
			},
		},
		{
			// The counter-case: hyphens with no space around them stay inside
			// the value.
			input: "region:eu-west-1 source:payment-worker",
			want: []Term{
				&FieldTerm{Key: "region", Values: []Value{{Text: "eu-west-1"}}},
				&FieldTerm{Key: "source", Values: []Value{{Text: "payment-worker"}}},
			},
		},
		{
			// A minus straight after a colon is part of the value.
			input: "offset:-5",
			want:  []Term{&FieldTerm{Key: "offset", Values: []Value{{Text: "-5"}}}},
		},
		{
			input: "on:2026-08-13",
			want:  []Term{&TimeTerm{Keyword: "on", Expr: "2026-08-13"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := mustParse(t, tt.input)
			if !reflect.DeepEqual(got.Terms, tt.want) {
				t.Errorf("Parse(%q)\n got: %s\nwant: %s",
					tt.input, describe(got.Terms), describe(tt.want))
			}
		})
	}
}
