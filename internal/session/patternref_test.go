package session

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// idFor returns the template id of the one template matching substring.
func idFor(t *testing.T, sess *Session, substring string) string {
	t.Helper()

	set, err := sess.Patterns(context.Background(), plan(t, sess, ""), PatternQuery{Limit: -1})
	if err != nil {
		t.Fatalf("Patterns: %v", err)
	}

	var found []string
	for _, p := range set.Patterns {
		if strings.Contains(p.Template, substring) {
			found = append(found, p.ID)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one template containing %q, got %v", substring, found)
	}
	return found[0]
}

// The whole point of the id: a template expands back to exactly its records.
// Without this the listing is a dead end.
func TestPatternTermExpandsToItsRecords(t *testing.T) {
	sess := patternFixture(t)
	ctx := context.Background()

	set, err := sess.Patterns(ctx, plan(t, sess, ""), PatternQuery{Limit: -1})
	if err != nil {
		t.Fatalf("Patterns: %v", err)
	}

	for _, p := range set.Patterns {
		got, err := sess.Count(ctx, plan(t, sess, "pattern:"+p.ID))
		if err != nil {
			t.Fatalf("count for %s (%q): %v", p.ID, p.Template, err)
		}
		if got != p.Count {
			t.Errorf("template %q listed %d records but pattern:%s matched %d",
				p.Template, p.Count, p.ID, got)
		}
	}
}

// A short id resolves, like a git short hash. Twelve hex characters is a lot
// to retype from a listing during an incident.
func TestPatternTermAcceptsAShortID(t *testing.T) {
	sess := patternFixture(t)
	ctx := context.Background()

	full := idFor(t, sess, "user <num> timed out")

	whole, err := sess.Count(ctx, plan(t, sess, "pattern:"+full))
	if err != nil {
		t.Fatalf("count: %v", err)
	}

	short, err := sess.Count(ctx, plan(t, sess, "pattern:"+full[:6]))
	if err != nil {
		t.Fatalf("count with a short id: %v", err)
	}

	if whole != short {
		t.Errorf("pattern:%s matched %d records but pattern:%s matched %d",
			full, whole, full[:6], short)
	}
}

// An id that is not in the data is a typo or a stale paste, never a question
// worth answering with an empty table.
func TestUnknownPatternIDIsAnError(t *testing.T) {
	sess := patternFixture(t)

	full := idFor(t, sess, "user <num> timed out")
	// Same length, last character changed: what a mistyped id looks like.
	wrong := full[:len(full)-1] + flipHex(full[len(full)-1])

	_, err := sess.Plan(context.Background(), "pattern:"+wrong)
	if err == nil {
		t.Fatal("an unknown template id planned without error")
	}

	var unknown *UnknownPatternError
	if !errors.As(err, &unknown) {
		t.Fatalf("error is %T, want *UnknownPatternError: %v", err, err)
	}

	// The suggestion is the point: a bad tail on a good prefix should find the
	// template that was meant.
	if len(unknown.Near) == 0 {
		t.Errorf("no suggestion offered for %q", wrong)
	} else if unknown.Near[0] != full {
		t.Errorf("suggested %v, want %s", unknown.Near, full)
	}
	if !strings.Contains(err.Error(), "loupe patterns") {
		t.Errorf("the error does not say how to list the templates: %v", err)
	}
}

// A prefix matching several templates must say so and list them rather than
// silently picking one.
func TestAmbiguousPatternIDIsAnError(t *testing.T) {
	// Seventeen distinct templates, so by the pigeonhole principle at least
	// two of the sixteen possible first hex characters must collide. A
	// fixture small enough to leave that to chance produced a test that
	// skipped itself, which is not a test.
	lines := make([]string, 0, 17)
	for i := 0; i < 17; i++ {
		lines = append(lines, `{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"distinct message `+
			string(rune('a'+i))+`"}`)
	}
	sess := openFixture(t, lines...)

	set, err := sess.Patterns(context.Background(), plan(t, sess, ""), PatternQuery{Limit: -1})
	if err != nil {
		t.Fatalf("Patterns: %v", err)
	}

	seen := map[byte][]string{}
	var prefix string
	for _, p := range set.Patterns {
		c := p.ID[0]
		seen[c] = append(seen[c], p.ID)
		if len(seen[c]) > 1 {
			prefix = string(c)
			break
		}
	}
	if prefix == "" {
		t.Fatalf("no two of %d templates share a first character, which is impossible",
			len(set.Patterns))
	}

	_, err = sess.Plan(context.Background(), "pattern:"+prefix)
	if err == nil {
		t.Fatalf("ambiguous id %q planned without error", prefix)
	}

	var ambiguous *AmbiguousPatternError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("error is %T, want *AmbiguousPatternError: %v", err, err)
	}
	if len(ambiguous.Candidates) < 2 {
		t.Errorf("only %d candidates listed", len(ambiguous.Candidates))
	}
	if !strings.Contains(err.Error(), "type more of the id") {
		t.Errorf("the error does not say how to disambiguate: %v", err)
	}
}

// A full id that happens to be a prefix of another must still resolve to
// itself rather than being called ambiguous.
func TestExactPatternIDWinsOverLongerOnes(t *testing.T) {
	sess := patternFixture(t)
	full := idFor(t, sess, "user <num> timed out")

	// Nothing extends a full-length id, so this asserts the exact-match branch
	// stays reachable rather than the prefix search shadowing it.
	got, err := sess.Count(context.Background(), plan(t, sess, "pattern:"+full))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got == 0 {
		t.Errorf("a full id matched nothing")
	}
}

// pattern: takes an id. Someone who pastes a template's text should be told
// that, not told their text is an id that does not exist.
func TestPatternTermRejectsTextThatIsNotAnID(t *testing.T) {
	sess := patternFixture(t)

	for _, input := range []string{
		`pattern:"user timed out"`,
		"pattern:timedout",
		"pattern:zzzzzz",
		// Longer than an id can be.
		"pattern:002cf356a676a11462dd2ea1",
	} {
		t.Run(input, func(t *testing.T) {
			_, err := sess.Plan(context.Background(), input)
			if err == nil {
				t.Fatalf("%q planned without error", input)
			}

			var malformed *MalformedPatternError
			if !errors.As(err, &malformed) {
				t.Fatalf("error is %T, want *MalformedPatternError: %v", err, err)
			}
			if !strings.Contains(err.Error(), "not a template's text") {
				t.Errorf("the error does not explain what pattern: takes: %v", err)
			}
		})
	}
}

// The existence tests keep working: pattern:none finds records with no
// template at all, and must not be mistaken for an id.
func TestPatternExistenceTermsAreNotResolved(t *testing.T) {
	sess := patternFixture(t)
	ctx := context.Background()

	for _, input := range []string{"pattern:none", "pattern:*"} {
		if _, err := sess.Plan(ctx, input); err != nil {
			t.Errorf("%q failed to plan: %v", input, err)
		}
	}

	// Every record gets a template, so none of them lack one.
	none, err := sess.Count(ctx, plan(t, sess, "pattern:none"))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if none != 0 {
		t.Errorf("%d records have no template; every record should get one", none)
	}
}

// Resolving a short id must not rewrite the query the user typed, which is
// what gets rendered back to them and explained on an empty result.
func TestResolvingAnIDLeavesTheParsedQueryAlone(t *testing.T) {
	sess := patternFixture(t)

	full := idFor(t, sess, "user <num> timed out")
	short := "pattern:" + full[:6]

	p, err := sess.Plan(context.Background(), short)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if got := p.Query.String(); got != short {
		t.Errorf("the reported query is %q, want the one that was typed, %q", got, short)
	}
}

// flipHex returns a different hex digit, for building a plausible typo.
func flipHex(c byte) string {
	if c == 'a' {
		return "b"
	}
	return "a"
}
