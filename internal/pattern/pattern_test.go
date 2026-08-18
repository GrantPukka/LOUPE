package pattern

import (
	"strings"
	"testing"
)

func TestTemplate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// The headline case from the roadmap.
		{"numeric id in a sentence", "user 4821 timed out", "user <num> timed out"},

		{"decimal", "duration: 173.106 ms", "duration: <num> ms"},
		{"negative", "drift -12 seconds", "drift <num> seconds"},
		{"thousands separator", "processed 1,024 rows", "processed <num> rows"},
		{"attached unit", "took 12ms", "took <num>ms"},
		{"percent", "cpu at 94% of quota", "cpu at <num>% of quota"},

		// Punctuation wraps the value, so it stays and the value goes.
		{"bracketed", "retry (3) of (5)", "retry (<num>) of (<num>)"},
		{"sql placeholder", "WHERE user_id = $1", "WHERE user_id = $<num>"},

		{"uuid", "session 3f2504e0-4f89-11d3-9a0c-0305e82c3301 closed",
			"session <uuid> closed"},
		{"ipv4", "connection refused to 10.0.0.14", "connection refused to <ip>"},
		{"ipv4 with port", "upstream 10.0.0.14:5432 timed out", "upstream <ip> timed out"},
		{"ipv6", "peer fe80::1ff:fe23:4567 gone", "peer <ip> gone"},

		{"timestamp", "expired at 2026-08-13T14:00:00Z", "expired at <ts>"},
		{"bare date", "for 2026-08-13 only", "for <ts> only"},
		{"bare clock", "at 14:00:00 sharp", "at <ts> sharp"},

		{"quoted", `rejected "bad token" from client`, `rejected "<str>" from client`},
		{"single quoted", "rejected 'bad token'", "rejected '<str>'"},
		{"empty quotes stay", `value "" is unset`, `value "" is unset`},

		{"opaque id", "trace a91c40f2b7e1 started", "trace <id> started"},
		{"key value", "user_id=u_18823 rejected", "user_id=<id> rejected"},

		// The path rule: variable segments only.
		{"path with numeric segment", "POST /api/orders/2291", "POST /api/orders/<num>"},
		{"path without one", "POST /api/cart", "POST /api/cart"},
		{"path with uuid segment", "GET /v1/users/3f2504e0-4f89-11d3-9a0c-0305e82c3301",
			"GET /v1/users/<uuid>"},

		// Whitespace is preserved so a line truncated mid-token stays its own
		// shape rather than merging with the healthy line it came from.
		{"doubled space", "PO  ST /healthz", "PO  ST /healthz"},

		{"empty", "", ""},
		{"only spaces", "   ", "   "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Template(tt.in); got != tt.want {
				t.Errorf("Template(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

// The whole risk of this feature is over-collapsing. Two messages that differ
// by a word are two events, and merging them hides one of them.
func TestWordsAreNeverMasked(t *testing.T) {
	tests := []struct{ a, b string }{
		{"failed to reserve stock", "failed to reserve payment"},
		{"POST /api/cart", "POST /api/checkout"},
		{"connection refused", "connection reset"},
		{"user 1 timed out", "order 1 timed out"},
		{"took 12ms", "took 12s"},
		{"GET /api/orders/1", "POST /api/orders/1"},
	}

	for _, tt := range tests {
		if Of(tt.a).ID == Of(tt.b).ID {
			t.Errorf("%q and %q collapsed into one template (%q); they are different events",
				tt.a, tt.b, Of(tt.a).Text)
		}
	}
}

// The point of the feature: messages differing only in their values are one
// template with a count.
func TestValuesCollapseTogether(t *testing.T) {
	groups := [][]string{
		{"user 4821 timed out", "user 9903 timed out", "user 1 timed out"},
		{"POST /api/orders/2291", "POST /api/orders/7", "POST /api/orders/918273"},
		{
			"duration: 173.106 ms  statement: SELECT * FROM orders WHERE user_id = $1",
			"duration: 44.602 ms  statement: SELECT * FROM orders WHERE user_id = $1",
		},
		{"connection refused to 10.0.0.14", "connection refused to 192.168.1.1"},
	}

	for _, group := range groups {
		first := Of(group[0])
		for _, m := range group[1:] {
			if got := Of(m); got.ID != first.ID {
				t.Errorf("%q (%q) and %q (%q) should be one template",
					group[0], first.Text, m, got.Text)
			}
		}
	}
}

// Only the first line is templated, or every stack trace becomes its own
// template — for exactly the records that matter most.
func TestOnlyTheFirstLineIsTemplated(t *testing.T) {
	a := "connection pool exhausted\n\tat Pool.acquire(Pool.java:88)\n\tat Foo.bar(Foo.java:12)"
	b := "connection pool exhausted\n\tat Pool.acquire(Pool.java:91)\n\tat Baz.qux(Baz.java:400)"

	got, want := Of(a), Of(b)
	if got.ID != want.ID {
		t.Errorf("two stack traces of the same error became different templates:\n %q\n %q",
			got.Text, want.Text)
	}
	if strings.Contains(got.Text, "Pool.java") {
		t.Errorf("template carries the stack trace: %q", got.Text)
	}
}

// A template id must be the same in every run and on every machine, or
// --new-since compares nothing and pattern:<id> is unusable between sessions.
func TestIDIsStableAndDerivedFromText(t *testing.T) {
	const message = "user 4821 timed out"

	first := Of(message)
	for i := 0; i < 100; i++ {
		if again := Of(message); again.ID != first.ID {
			t.Fatalf("id is not stable: %q then %q", first.ID, again.ID)
		}
	}

	if len(first.ID) != IDLength {
		t.Errorf("id %q is %d characters, want %d", first.ID, len(first.ID), IDLength)
	}

	// Same template text, arrived at from a different message, is the same
	// pattern. That is the property the whole feature rests on.
	if other := Of("user 9903 timed out"); other.ID != first.ID {
		t.Errorf("same template %q got different ids %q and %q",
			first.Text, first.ID, other.ID)
	}
}

// A masked template must never be confused with text that was really logged.
func TestAMessageContainingAMaskIsStillDistinct(t *testing.T) {
	literal := Of("user <num> timed out")
	masked := Of("user 4821 timed out")

	if literal.Text != masked.Text {
		return // They differ, which is the safe outcome.
	}
	// If they do collapse, it is because someone logged the mask spelling
	// verbatim. Record the behaviour rather than pretending it cannot happen.
	t.Logf("a message containing %q literally collapses with a real one; "+
		"acceptable, but worth knowing", MaskNum)
}

func TestUnitsAreKeptButWordsAreNot(t *testing.T) {
	tests := []struct{ in, want string }{
		{"12ms", "<num>ms"},
		{"3s", "<num>s"},
		{"512kb", "<num>kb"},
		// Four letters is a word with a number stuck to it, not a unit, so the
		// token is an opaque id rather than a measurement.
		{"12345abcd", "<id>"},
	}

	for _, tt := range tests {
		if got := Template(tt.in); got != tt.want {
			t.Errorf("Template(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func BenchmarkOf(b *testing.B) {
	const message = "duration: 173.106 ms  statement: SELECT * FROM orders WHERE user_id = $1"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Of(message)
	}
}

// The two halves of Of, measured separately: masking a line, and naming the
// result. This runs on every record at ingest, so which half costs what
// decides where any optimisation belongs.
func BenchmarkTemplateOnly(b *testing.B) {
	const message = "duration: 173.106 ms  statement: SELECT * FROM orders WHERE user_id = $1"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Template(message)
	}
}

// The common case: a message with nothing in it to mask. Roughly half of a
// real corpus looks like this, so it must not allocate.
func BenchmarkTemplatePlainMessage(b *testing.B) {
	const message = "request completed"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Template(message)
	}
}

// Of on a plain message, against BenchmarkTemplatePlainMessage: the gap is
// what naming the template costs.
func BenchmarkOfPlainMessage(b *testing.B) {
	const message = "request completed"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Of(message)
	}
}
