package session

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// patternFixture is a directory whose messages differ only in their values,
// which is the case the whole feature exists for.
func patternFixture(t *testing.T) *Session {
	t.Helper()

	return openFixture(t,
		`{"ts":"2026-08-13T14:00:00Z","level":"error","msg":"user 4821 timed out","status":500}`,
		`{"ts":"2026-08-13T14:01:00Z","level":"error","msg":"user 9903 timed out","status":500}`,
		`{"ts":"2026-08-13T14:02:00Z","level":"error","msg":"user 12 timed out","status":500}`,
		`{"ts":"2026-08-13T14:03:00Z","level":"info","msg":"request completed","status":200}`,
		`{"ts":"2026-08-13T14:04:00Z","level":"info","msg":"request completed","status":200}`,
		`{"ts":"2026-08-13T14:05:00Z","level":"warn","msg":"failed to reserve stock","status":409}`,
		`{"level":"error","msg":"user 77 timed out","status":500}`,
		`this line is not json`,
	)
}

// byTemplate indexes a listing so a test can assert on one template without
// depending on where it landed.
func byTemplate(set PatternSet) map[string]Pattern {
	out := map[string]Pattern{}
	for _, p := range set.Patterns {
		out[p.Template] = p
	}
	return out
}

func TestPatternsGroupsByTemplate(t *testing.T) {
	sess := patternFixture(t)
	ctx := context.Background()

	set, err := sess.Patterns(ctx, plan(t, sess, ""), PatternQuery{})
	if err != nil {
		t.Fatalf("Patterns: %v", err)
	}

	found := byTemplate(set)

	// Four messages differ only in a number, so they are one template — and
	// the one with no timestamp is still counted.
	timedOut, ok := found["user <num> timed out"]
	if !ok {
		t.Fatalf("no 'user <num> timed out' template in %v", found)
	}
	if timedOut.Count != 4 {
		t.Errorf("count = %d, want 4", timedOut.Count)
	}
	if timedOut.NoTimestamp != 1 {
		t.Errorf("no_timestamp = %d, want 1", timedOut.NoTimestamp)
	}

	// First and Last describe the dated records only, and must not be dragged
	// to zero by the undated one.
	wantFirst := time.Date(2026, 8, 13, 14, 0, 0, 0, time.UTC)
	if !timedOut.First.Equal(wantFirst) {
		t.Errorf("first = %s, want %s", timedOut.First, wantFirst)
	}

	if completed := found["request completed"]; completed.Count != 2 {
		t.Errorf("'request completed' count = %d, want 2", completed.Count)
	}

	// A message that differs by a word is its own template.
	if _, ok := found["failed to reserve stock"]; !ok {
		t.Error("'failed to reserve stock' was collapsed into something else")
	}

	if set.Records != 8 {
		t.Errorf("records = %d, want 8", set.Records)
	}
}

// An example must be a message the template was actually built from, so the
// masking can be checked against something real.
func TestPatternsCarryAnExampleAndItsSources(t *testing.T) {
	sess := patternFixture(t)

	set, err := sess.Patterns(context.Background(), plan(t, sess, ""), PatternQuery{})
	if err != nil {
		t.Fatalf("Patterns: %v", err)
	}

	for _, p := range set.Patterns {
		if p.Example == "" {
			t.Errorf("template %q has no example", p.Template)
		}
		if len(p.Sources) == 0 {
			t.Errorf("template %q lists no sources", p.Template)
		}
		for _, s := range p.Sources {
			if s == "" {
				t.Errorf("template %q has an empty source name", p.Template)
			}
		}
	}
}

// The line no parser understood must still appear, templated from its raw
// text. Dropping it would hide the record most in need of a look.
func TestPatternsIncludeUnparsedRecords(t *testing.T) {
	sess := patternFixture(t)

	set, err := sess.Patterns(context.Background(), plan(t, sess, ""), PatternQuery{})
	if err != nil {
		t.Fatalf("Patterns: %v", err)
	}

	if _, ok := byTemplate(set)["this line is not json"]; !ok {
		t.Errorf("the unparsed line has no template; got %v", byTemplate(set))
	}

	// And the listing says how much of itself came from broken lines, because
	// those cluster nothing like parsed messages do.
	if set.UnparsedTemplates != 1 || set.UnparsedRecords != 1 {
		t.Errorf("unparsed = %d templates / %d records, want 1 and 1",
			set.UnparsedTemplates, set.UnparsedRecords)
	}
}

// Truncation is always declared, and what is missing is counted.
func TestPatternsReportWhatTheLimitHid(t *testing.T) {
	sess := patternFixture(t)

	full, err := sess.Patterns(context.Background(), plan(t, sess, ""), PatternQuery{Limit: -1})
	if err != nil {
		t.Fatalf("Patterns: %v", err)
	}

	set, err := sess.Patterns(context.Background(), plan(t, sess, ""), PatternQuery{Limit: 2})
	if err != nil {
		t.Fatalf("Patterns: %v", err)
	}

	if len(set.Patterns) != 2 {
		t.Fatalf("got %d patterns, want 2", len(set.Patterns))
	}
	if !set.Truncated {
		t.Error("a cut listing did not report itself truncated")
	}
	if want := int64(len(full.Patterns) - 2); set.Hidden != want {
		t.Errorf("hidden = %d, want %d", set.Hidden, want)
	}

	// The counts must add up, or the footer understates the data.
	var shown int64
	for _, p := range set.Patterns {
		shown += p.Count
	}
	if shown+set.HiddenRecords != set.Records {
		t.Errorf("%d shown + %d hidden != %d records",
			shown, set.HiddenRecords, set.Records)
	}
}

// Most frequent first, with a deterministic tiebreak. A listing that reordered
// itself between runs could not be trusted or tested.
func TestPatternsAreOrderedAndStable(t *testing.T) {
	sess := patternFixture(t)
	ctx := context.Background()

	var first []string
	for i := 0; i < 5; i++ {
		set, err := sess.Patterns(ctx, plan(t, sess, ""), PatternQuery{Limit: -1})
		if err != nil {
			t.Fatalf("Patterns: %v", err)
		}

		var order []string
		var previous int64 = 1 << 62
		for _, p := range set.Patterns {
			if p.Count > previous {
				t.Fatalf("template %q (%d) sorted after a smaller count (%d)",
					p.Template, p.Count, previous)
			}
			previous = p.Count
			order = append(order, p.ID)
		}

		if first == nil {
			first = order
			continue
		}
		if fmt.Sprint(order) != fmt.Sprint(first) {
			t.Fatalf("listing reordered between runs:\n %v\n %v", first, order)
		}
	}
}

// A filter narrows the listing exactly as it narrows a record query.
func TestPatternsRespectTheFilter(t *testing.T) {
	sess := patternFixture(t)

	set, err := sess.Patterns(context.Background(), plan(t, sess, "level:error"), PatternQuery{})
	if err != nil {
		t.Fatalf("Patterns: %v", err)
	}

	found := byTemplate(set)
	if _, ok := found["request completed"]; ok {
		t.Error("an info template survived level:error")
	}
	if timedOut := found["user <num> timed out"]; timedOut.Count != 4 {
		t.Errorf("count = %d, want 4", timedOut.Count)
	}
}

// --new-since is the feature: which shapes have started happening.
func TestPatternsNewSince(t *testing.T) {
	sess := openFixture(t,
		// Established: present on both sides of the cutoff.
		`{"ts":"2026-08-13T13:00:00Z","level":"info","msg":"request completed"}`,
		`{"ts":"2026-08-13T14:09:00Z","level":"info","msg":"request completed"}`,
		// Only before: must not be called new, and must not be listed.
		`{"ts":"2026-08-13T13:30:00Z","level":"warn","msg":"cache miss for key 12"}`,
		// Only after: this is the answer.
		`{"ts":"2026-08-13T14:08:00Z","level":"error","msg":"pool exhausted after 30s"}`,
		`{"ts":"2026-08-13T14:10:00Z","level":"error","msg":"pool exhausted after 45s"}`,
	)

	// The newest record is 14:10, so a five-minute window cuts at 14:05.
	set, err := sess.Patterns(context.Background(), plan(t, sess, ""),
		PatternQuery{NewSince: 5 * time.Minute, Limit: -1})
	if err != nil {
		t.Fatalf("Patterns: %v", err)
	}

	if len(set.Patterns) != 1 {
		t.Fatalf("got %d new templates, want 1: %v", len(set.Patterns), byTemplate(set))
	}

	only := set.Patterns[0]
	if only.Template != "pool exhausted after <num>s" {
		t.Errorf("new template = %q, want the pool one", only.Template)
	}
	if only.Count != 2 {
		t.Errorf("count = %d, want 2", only.Count)
	}
	if !only.New || only.Before != 0 {
		t.Errorf("template is not marked new: new=%v before=%d", only.New, only.Before)
	}

	// The two it left out are counted, or a filtered listing is
	// indistinguishable from a quiet dataset.
	if set.Established != 2 {
		t.Errorf("established = %d, want 2", set.Established)
	}

	// The cutoff and what it counted back from are both reported, so the
	// window can be stated in local and UTC before any result.
	if set.Since.IsZero() {
		t.Error("no cutoff reported")
	}
	if want := time.Date(2026, 8, 13, 14, 5, 0, 0, time.UTC); !set.Since.Equal(want) {
		t.Errorf("cutoff = %s, want %s", set.Since, want)
	}
	if set.Anchor == "" {
		t.Error("the cutoff does not say what it counted back from")
	}
}

// A record with no timestamp sits on neither side of the cutoff. It must be
// counted and reported rather than quietly making a template look new.
func TestPatternsNewSinceReportsUndatedRecords(t *testing.T) {
	sess := openFixture(t,
		`{"ts":"2026-08-13T13:00:00Z","level":"info","msg":"request completed"}`,
		`{"ts":"2026-08-13T14:10:00Z","level":"error","msg":"pool exhausted"}`,
		`{"level":"error","msg":"undated and unplaceable"}`,
	)

	set, err := sess.Patterns(context.Background(), plan(t, sess, ""),
		PatternQuery{NewSince: 5 * time.Minute, Limit: -1})
	if err != nil {
		t.Fatalf("Patterns: %v", err)
	}

	if set.Undated != 1 {
		t.Errorf("undated = %d, want 1", set.Undated)
	}
}

// An empty result is an ordinary answer and must not look like a failure.
func TestPatternsOnAnEmptyResult(t *testing.T) {
	sess := patternFixture(t)

	set, err := sess.Patterns(context.Background(),
		plan(t, sess, "level:fatal"), PatternQuery{})
	if err != nil {
		t.Fatalf("Patterns: %v", err)
	}

	if set.Templates != 0 || len(set.Patterns) != 0 {
		t.Errorf("expected an empty listing, got %d templates", set.Templates)
	}
	if set.Truncated {
		t.Error("an empty listing reported itself truncated")
	}
}
