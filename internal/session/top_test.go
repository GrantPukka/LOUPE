package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GrantPukka/loupe/internal/query"
)

// topFixture has a promoted field, a bag-only field, records missing the field
// entirely, and one empty value — the four cases a breakdown has to handle.
func topFixture(t *testing.T) *Session {
	t.Helper()

	return openFixture(t,
		`{"ts":"2026-08-13T14:00:00Z","level":"error","msg":"a","path":"/api/checkout","status":500}`,
		`{"ts":"2026-08-13T14:00:01Z","level":"error","msg":"b","path":"/api/checkout","status":500}`,
		`{"ts":"2026-08-13T14:00:02Z","level":"error","msg":"c","path":"/api/checkout","status":503}`,
		`{"ts":"2026-08-13T14:00:03Z","level":"warn","msg":"d","path":"/api/cart","status":429}`,
		`{"ts":"2026-08-13T14:00:04Z","level":"info","msg":"e","path":"/healthz","status":200}`,
		// Present but empty: a real answer that must not render as nothing.
		`{"ts":"2026-08-13T14:00:05Z","level":"info","msg":"f","path":"","status":200}`,
		// No path at all: outside the breakdown, and it has to be reported.
		`{"ts":"2026-08-13T14:00:06Z","level":"info","msg":"g","status":200}`,
		`{"ts":"2026-08-13T14:00:07Z","level":"info","msg":"h","status":200}`,
	)
}

func topOf(t *testing.T, sess *Session, field, filter string, limit int) TopSet {
	t.Helper()

	got, err := sess.Top(context.Background(), plan(t, sess, filter),
		TopQuery{Field: field, Limit: limit})
	if err != nil {
		t.Fatalf("Top(%q, %q): %v", field, filter, err)
	}
	return got
}

// The headline: value counts, most frequent first.
func TestTopCountsValuesDescending(t *testing.T) {
	got := topOf(t, topFixture(t), "path", "", -1)

	if len(got.Values) != 4 {
		t.Fatalf("got %d values, want 4: %+v", len(got.Values), got.Values)
	}
	if got.Values[0].Value != "/api/checkout" || got.Values[0].Count != 3 {
		t.Errorf("first value = %+v, want /api/checkout with 3", got.Values[0])
	}

	previous := int64(1 << 62)
	for _, v := range got.Values {
		if v.Count > previous {
			t.Errorf("value %q (%d) sorted after a smaller count (%d)", v.Value, v.Count, previous)
		}
		previous = v.Count
	}
}

// The share is of the records carrying the field, so the values sum to one and
// the breakdown reads as a distribution.
func TestTopSharesSumToOne(t *testing.T) {
	got := topOf(t, topFixture(t), "path", "", -1)

	var total float64
	for _, v := range got.Values {
		total += v.Share
	}
	if total < 0.999 || total > 1.001 {
		t.Errorf("shares sum to %v, want 1", total)
	}

	// And the denominator is the records that carry the field, not everything
	// the filter matched.
	if got.Present != 6 {
		t.Errorf("present = %d, want 6", got.Present)
	}
	if want := 3.0 / 6.0; got.Values[0].Share != want {
		t.Errorf("top share = %v, want %v", got.Values[0].Share, want)
	}
}

// Records missing the field are outside the percentages, so they have to be
// counted and stated rather than quietly dropped from the denominator.
func TestTopReportsRecordsMissingTheField(t *testing.T) {
	got := topOf(t, topFixture(t), "path", "", -1)

	if got.Matched != 8 {
		t.Errorf("matched = %d, want 8", got.Matched)
	}
	if got.Absent != 2 {
		t.Errorf("absent = %d, want 2", got.Absent)
	}
	if got.Present+got.Absent != got.Matched {
		t.Errorf("%d present + %d absent != %d matched", got.Present, got.Absent, got.Matched)
	}
}

// A field present but empty is a real value, not an absence.
func TestTopKeepsAnEmptyValue(t *testing.T) {
	got := topOf(t, topFixture(t), "path", "", -1)

	found := false
	for _, v := range got.Values {
		if v.Value == "" {
			found = true
			if v.Count != 1 {
				t.Errorf("the empty value has count %d, want 1", v.Count)
			}
		}
	}
	if !found {
		t.Errorf("the empty path was treated as absent: %+v", got.Values)
	}
}

// Truncation is declared, and what it hid is counted.
func TestTopReportsWhatTheLimitHid(t *testing.T) {
	sess := topFixture(t)

	full := topOf(t, sess, "path", "", -1)
	got := topOf(t, sess, "path", "", 2)

	if len(got.Values) != 2 {
		t.Fatalf("got %d values, want 2", len(got.Values))
	}
	if !got.Truncated {
		t.Error("a cut breakdown did not report itself truncated")
	}
	if want := int64(len(full.Values) - 2); got.Hidden != want {
		t.Errorf("hidden = %d, want %d", got.Hidden, want)
	}

	// The counts must add up, or the footer understates the data.
	var shown int64
	for _, v := range got.Values {
		shown += v.Count
	}
	if shown+got.HiddenRecords != got.Present {
		t.Errorf("%d shown + %d hidden != %d present", shown, got.HiddenRecords, got.Present)
	}
}

// A filter narrows the breakdown exactly as it narrows a record listing.
func TestTopRespectsTheFilter(t *testing.T) {
	got := topOf(t, topFixture(t), "path", "level:error", -1)

	if len(got.Values) != 1 {
		t.Fatalf("got %d values, want just the errors' path: %+v", len(got.Values), got.Values)
	}
	if got.Values[0].Value != "/api/checkout" || got.Values[0].Count != 3 {
		t.Errorf("value = %+v, want /api/checkout with 3", got.Values[0])
	}
	if got.Absent != 0 {
		t.Errorf("absent = %d, want 0 — every error record has a path", got.Absent)
	}
}

// Works on a promoted column and on a built-in one alike, because it resolves
// the field through the same code the filter compiler uses.
func TestTopWorksOnPromotedAndBuiltInFields(t *testing.T) {
	sess := topFixture(t)

	// status is promoted to a typed column by schema inference.
	status := topOf(t, sess, "status", "", -1)
	if len(status.Values) == 0 {
		t.Fatal("no status values")
	}
	if status.Values[0].Value != "200" {
		t.Errorf("most common status = %q, want 200", status.Values[0].Value)
	}

	// level is a built-in column.
	level := topOf(t, sess, "level", "", -1)
	if len(level.Values) != 3 {
		t.Errorf("got %d levels, want 3: %+v", len(level.Values), level.Values)
	}
}

// A typo gets the same spelling suggestion it would get in a filter, never an
// empty breakdown.
func TestTopRejectsAnUnknownField(t *testing.T) {
	sess := topFixture(t)

	_, err := sess.Top(context.Background(), plan(t, sess, ""), TopQuery{Field: "pth"})
	if err == nil {
		t.Fatal("an unknown field produced a breakdown")
	}

	var unknown *query.UnknownFieldError
	if !errors.As(err, &unknown) {
		t.Fatalf("error is %T, want *query.UnknownFieldError: %v", err, err)
	}
	if !strings.Contains(err.Error(), "path") {
		t.Errorf("error does not suggest the field meant: %v", err)
	}
}

func TestTopNeedsAField(t *testing.T) {
	sess := topFixture(t)

	if _, err := sess.Top(context.Background(), plan(t, sess, ""), TopQuery{}); err == nil {
		t.Error("a breakdown with no field was accepted")
	}
}

// Ordering must be stable, or two breakdowns taken a minute apart cannot be
// compared.
func TestTopIsStable(t *testing.T) {
	sess := topFixture(t)

	first := topOf(t, sess, "path", "", -1)
	for i := 0; i < 5; i++ {
		again := topOf(t, sess, "path", "", -1)
		for j := range first.Values {
			if again.Values[j] != first.Values[j] {
				t.Fatalf("breakdown reordered between runs at %d: %+v vs %+v",
					j, first.Values[j], again.Values[j])
			}
		}
	}
}

// Nothing matched, and nothing carrying the field, are different answers and
// neither is an error.
func TestTopOnEmptyResults(t *testing.T) {
	sess := topFixture(t)

	nothing := topOf(t, sess, "path", "level:fatal", -1)
	if nothing.Matched != 0 || len(nothing.Values) != 0 {
		t.Errorf("expected an empty breakdown, got %+v", nothing)
	}

	// Records matched, but none of them carry the field.
	absent := topOf(t, sess, "path", "msg:g", -1)
	if absent.Matched == 0 {
		t.Fatal("the filter matched nothing, so this tests the wrong thing")
	}
	if absent.Present != 0 || len(absent.Values) != 0 {
		t.Errorf("expected no values, got %+v", absent)
	}
	if absent.Absent != absent.Matched {
		t.Errorf("absent = %d, want all %d matched", absent.Absent, absent.Matched)
	}
}

// sshFixture writes the two shapes sshd actually uses. Counting the phrase
// "Failed password for root" finds only the first and undercounts by 42%,
// looking like a clean confident answer — the failure `top` exists to prevent,
// except that the username lives in unparsed text where there is no field.
func sshFixture(t *testing.T) *Session {
	t.Helper()

	return openFixture(t,
		`Aug 13 14:00:00 host sshd[1001]: Failed password for root from 10.0.0.1 port 2222 ssh2`,
		`Aug 13 14:00:01 host sshd[1002]: Failed password for invalid user root from 10.0.0.2 port 2222 ssh2`,
		`Aug 13 14:00:02 host sshd[1003]: Failed password for invalid user root from 10.0.0.3 port 2222 ssh2`,
		`Aug 13 14:00:03 host sshd[1004]: Failed password for admin from 10.0.0.4 port 2222 ssh2`,
		`Aug 13 14:00:04 host sshd[1005]: Accepted password for deploy from 10.0.0.5 port 2222 ssh2`,
	)
}

func TestTopByRegexCapture(t *testing.T) {
	got := topOf(t, sshFixture(t), `/Failed password for (?:invalid user )?(\S+)/`, "", -1)

	want := map[string]int64{"root": 3, "admin": 1}
	if len(got.Values) != len(want) {
		t.Fatalf("values = %+v, want %d of them", got.Values, len(want))
	}
	for _, v := range got.Values {
		if want[v.Value] != v.Count {
			t.Errorf("%s = %d, want %d", v.Value, v.Count, want[v.Value])
		}
	}

	// The line that did not match is outside the breakdown and has to be said
	// so, not folded into the denominator.
	if got.Absent != 1 {
		t.Errorf("Absent = %d, want 1 — the Accepted line matches no capture", got.Absent)
	}
}

// A pattern with no capture group means the whole match.
func TestTopByRegexWithoutACapture(t *testing.T) {
	got := topOf(t, sshFixture(t), `/(?:Failed|Accepted) password/`, "", -1)

	counts := map[string]int64{}
	for _, v := range got.Values {
		counts[v.Value] = v.Count
	}
	if counts["Failed password"] != 4 || counts["Accepted password"] != 1 {
		t.Errorf("values = %+v, want 4 Failed and 1 Accepted", got.Values)
	}
}

// The field~/regex/ form targets a column, spelled the way the filter language
// spells a regex.
func TestTopByRegexOnANamedField(t *testing.T) {
	got := topOf(t, sshFixture(t), `message~/for (?:invalid user )?(\S+)/`, "", -1)

	if len(got.Values) == 0 {
		t.Fatal("no values extracted from message")
	}
	for _, v := range got.Values {
		if strings.ContainsAny(v.Value, " ") {
			t.Errorf("captured %q, want a single username", v.Value)
		}
	}
}

// A bad pattern is an error before anything runs, not an empty table.
func TestTopByInvalidRegex(t *testing.T) {
	sess := sshFixture(t)

	_, err := sess.Top(context.Background(), plan(t, sess, ""),
		TopQuery{Field: `/unclosed (group/`, Limit: -1})
	if err == nil {
		t.Fatal("an invalid regex should be an error")
	}
	if !strings.Contains(err.Error(), "invalid regex") {
		t.Errorf("error = %v, want it to name the problem", err)
	}
}

// The regex is a parameter, never concatenated into the statement.
func TestTopRegexIsParameterised(t *testing.T) {
	expr, err := topExprFor(query.Schema{}, `/x(y)/`)
	if err != nil {
		t.Fatalf("topExprFor: %v", err)
	}
	if strings.Contains(expr.SQL, "x(y)") {
		t.Errorf("the pattern was built into the SQL: %s", expr.SQL)
	}
	if len(expr.Args) != 1 || expr.Args[0] != "x(y)" {
		t.Errorf("Args = %#v, want the pattern as a parameter", expr.Args)
	}
}
