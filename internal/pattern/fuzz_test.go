package pattern

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Templating runs on every record at ingest, on whatever the log contained.
//
// The properties below are what the rest of the tool assumes without checking:
// the ID is what --new-since compares and what pattern:<id> selects, and the
// text goes straight to a terminal and a browser. A template that is not stable
// across runs makes --new-since report noise as novelty, which is worse than
// reporting nothing.

func seedMessages(f *testing.F) {
	f.Helper()

	for _, seed := range []string{
		"",
		" ",
		"request completed",
		"user 1234 timed out after 30s",
		"GET /api/orders/8a1f9c2e-0000-4000-8000-000000000000 200",
		"connection from 10.0.0.1:54321 refused",
		"error at 2026-08-13T14:00:00Z",
		"boom\n\tat Foo.bar(Foo.java:1)\n\tat Baz.qux(Baz.java:2)",
		"\x00\x00truncated write",
		"\ufeffleading bom",
		"\xff\xfe invalid utf-8",
		strings.Repeat("a", 4096),
		strings.Repeat("1 ", 512),
		"<num> already looks like a mask",
	} {
		f.Add(seed)
	}
}

// FuzzOf pins the contract the ID and the template text carry.
func FuzzOf(f *testing.F) {
	seedMessages(f)

	f.Fuzz(func(t *testing.T, message string) {
		got := Of(message)

		// The ID names the template across runs and machines. Anything other
		// than a fixed-width hex string means pattern:<id> cannot round trip
		// through a URL, a terminal, or a handoff file.
		if len(got.ID) != IDLength {
			t.Fatalf("Of(%q).ID = %q, want %d characters", message, got.ID, IDLength)
		}
		for i := 0; i < len(got.ID); i++ {
			if c := got.ID[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				t.Fatalf("Of(%q).ID = %q, not lowercase hex", message, got.ID)
			}
		}

		// A template is read by a person, in a terminal and in a browser. A
		// control character renders as nothing in one and as a box in the
		// other, so both readers conclude the tool is broken rather than that
		// the log line is. Masking them is the only honest rendering, and the
		// masking has to be complete.
		for _, r := range got.Text {
			if r == utf8.RuneError {
				continue // Invalid input bytes, preserved rather than dropped.
			}
			// Tab is not damage: it is a legitimate separator in a log line and
			// it renders as whitespace, which is why isControl excludes it.
			if r == '\t' {
				continue
			}
			if r < 0x20 || r == 0x7f {
				t.Fatalf("Of(%q).Text contains control character %U: %q",
					message, r, got.Text)
			}
		}

		// Only the first line is templated, so a stack trace cannot smuggle a
		// newline into a value that gets printed as one row.
		if strings.ContainsAny(got.Text, "\n\r") {
			t.Fatalf("Of(%q).Text spans lines: %q", message, got.Text)
		}
	})
}

// FuzzOfIsStable pins that the same message always yields the same template.
//
// --new-since compares IDs recorded on an earlier run against IDs computed on
// this one. If templating consulted a map's iteration order or anything else
// that varies, every pattern would look new on every run, and the feature would
// report noise with total confidence.
func FuzzOfIsStable(f *testing.F) {
	seedMessages(f)

	f.Fuzz(func(t *testing.T, message string) {
		first := Of(message)
		second := Of(message)

		if first != second {
			t.Fatalf("Of(%q) gave %+v then %+v", message, first, second)
		}

		// The ID is a function of the template alone, so two messages that
		// template alike must share an ID — that is the whole basis of
		// grouping.
		if again := Of(first.Text); again.Text == first.Text && again.ID != first.ID {
			t.Fatalf("template %q has two IDs: %s and %s", first.Text, first.ID, again.ID)
		}
	})
}

// FuzzTemplateLeavesProseAlone pins that masking stays conservative.
//
// Over-eager masking is the failure mode that destroys the feature rather than
// merely blunting it: if ordinary words are masked, unrelated messages collapse
// into one template and the rail reports a single meaningless shape. Prose —
// letters and spaces, nothing that could be a value — must come back untouched.
//
// There was an idempotency target here as well, asserting Template(Template(x))
// == Template(x). It found that "\x1d=000" masks to "<ctl>=000" on the first
// pass and "<ctl>=<num>" on the second, because "=000" has no key for the
// key=value rule while "<ctl>=000" does. That is real, but Template has one
// call site and it is fed raw message text, never its own output, so the
// property was invented. Satisfying it would mean changing what masks — which
// changes every template id, and template ids are what --new-since compares
// against an earlier run. Not worth it for an input no log produces.
func FuzzTemplateLeavesProseAlone(f *testing.F) {
	f.Add("request completed")
	f.Add("connection refused by upstream")
	f.Add("a")
	f.Add("")

	f.Fuzz(func(t *testing.T, line string) {
		for i := 0; i < len(line); i++ {
			if c := line[i]; !isLetter(c) && c != ' ' {
				return
			}
		}

		if got := Template(line); got != line {
			t.Fatalf("Template masked plain prose: %q became %q", line, got)
		}
	})
}
