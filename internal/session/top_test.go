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
