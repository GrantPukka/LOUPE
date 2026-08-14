package session

import (
	"context"
	"strings"
	"testing"
)

func handoffFixture(t *testing.T) *Session {
	t.Helper()
	return openFixture(t,
		`{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"a","status":200,"user_id":"u_1"}`,
		`{"ts":"2026-08-13T14:01:00Z","level":"error","msg":"boom","status":502,"user_id":"u_2"}`,
		`{"ts":"2026-08-13T14:02:00Z","level":"error","msg":"bang","status":500,"user_id":"u_2"}`,
		`{"ts":"2026-08-13T14:03:00Z","level":"error","msg":"crash","status":500,"user_id":"u_3"}`,
		`{"level":"error","msg":"no timestamp","status":500}`,
		`not json at all`,
	)
}

func build(t *testing.T, s *Session, filter string, opts HandoffOptions) Handoff {
	t.Helper()

	p := plan(t, s, filter)
	out, err := s.Handoff(context.Background(), p, opts)
	if err != nil {
		t.Fatalf("Handoff(%q): %v", filter, err)
	}
	return out
}

// The extract must be the same records the display path would show. Same AST,
// same SQL, different renderer.
func TestHandoffMatchesTheDisplayPath(t *testing.T) {
	s := handoffFixture(t)
	ctx := context.Background()

	p := plan(t, s, "level:error")

	displayed, err := s.Records(ctx, p, RecordQuery{})
	if err != nil {
		t.Fatalf("Records: %v", err)
	}

	extract := build(t, s, "level:error", HandoffOptions{})

	if int(extract.Counts.Shown) != displayed.RowCount() {
		t.Errorf("extract has %d records, the display had %d",
			extract.Counts.Shown, displayed.RowCount())
	}
	if extract.Counts.Matched != displayed.Total {
		t.Errorf("extract matched %d, the display matched %d",
			extract.Counts.Matched, displayed.Total)
	}

	// And the same messages, in the same order.
	for i, record := range extract.Records {
		want, _ := displayed.Rows[i][3].(string)
		if record.Text != want {
			t.Errorf("record %d: extract has %q, the display had %q", i, record.Text, want)
		}
	}
}

// Truncation is always declared, with the real total. An extract that does not
// say it is truncated is worse than no extract.
func TestHandoffDeclaresTruncation(t *testing.T) {
	s := handoffFixture(t)

	extract := build(t, s, "level:error", HandoffOptions{Limit: 2})

	if !extract.Truncated {
		t.Fatal("a limited extract did not report truncation")
	}
	if extract.Counts.Shown != 2 {
		t.Errorf("Shown = %d, want 2", extract.Counts.Shown)
	}
	if extract.Counts.Matched != 4 {
		t.Errorf("Matched = %d, want the full 4", extract.Counts.Matched)
	}
}

func TestHandoffUntruncatedSaysSo(t *testing.T) {
	s := handoffFixture(t)

	extract := build(t, s, "level:error", HandoffOptions{Limit: 100})
	if extract.Truncated {
		t.Error("an extract holding every match reported truncation")
	}
}

// A time filter excludes untimestamped records, and the count travels with the
// extract. Silence about excluded data is how handoffs mislead.
func TestHandoffReportsExcludedRecords(t *testing.T) {
	s := handoffFixture(t)

	extract := build(t, s, "after:2026-08-13T00:00:00Z level:error", HandoffOptions{})

	if extract.Counts.ExcludedNoTimestamp == 0 {
		t.Error("a time filter excluded untimestamped records without saying so")
	}
	if extract.Counts.Unparsed == 0 {
		t.Error("the unparsed count is missing")
	}
	if extract.Counts.Ingested == 0 {
		t.Error("the denominator is missing; a reader cannot judge the numbers")
	}
}

// Both readings of the window, so nobody does offset arithmetic at either end.
func TestHandoffWindowInBothZones(t *testing.T) {
	s := handoffFixture(t)

	extract := build(t, s, "between:2026-08-13T14:00:00Z-2026-08-13T14:02:00Z", HandoffOptions{})

	if extract.WindowLocal == "" || extract.WindowUTC == "" {
		t.Fatalf("window not rendered in both zones: %q / %q",
			extract.WindowLocal, extract.WindowUTC)
	}
	if !strings.Contains(extract.WindowUTC, "UTC") {
		t.Errorf("the UTC window does not name the zone: %q", extract.WindowUTC)
	}
	if extract.WindowStart.IsZero() || extract.WindowEnd.IsZero() {
		t.Error("the machine-readable window bounds are missing")
	}
}

// The raw line is always included. The receiver may not trust our parser.
func TestHandoffAlwaysIncludesRawLines(t *testing.T) {
	s := handoffFixture(t)

	extract := build(t, s, "level:error", HandoffOptions{})
	if len(extract.Records) == 0 {
		t.Fatal("no records")
	}

	for i, record := range extract.Records {
		if record.Raw == "" {
			t.Errorf("record %d has no raw line", i)
		}
	}
}

// Every source is listed with whether its timezone was known or assumed. A
// receiver who does not know an assumption was made cannot check it.
func TestHandoffListsSourcesAndTimezoneProvenance(t *testing.T) {
	s := handoffFixture(t)

	extract := build(t, s, "", HandoffOptions{})
	if len(extract.Sources) == 0 {
		t.Fatal("no sources listed")
	}
	for _, source := range extract.Sources {
		if source.Timezone == "" {
			t.Errorf("%s has no timezone verdict", source.File)
		}
		if source.Format == "" {
			t.Errorf("%s has no format", source.File)
		}
	}
}

// Redaction is opt-in. Silently altering evidence is worse than exposing it.
func TestHandoffDoesNotRedactByDefault(t *testing.T) {
	s := handoffFixture(t)

	extract := build(t, s, "level:error", HandoffOptions{})
	if len(extract.Redacted) != 0 {
		t.Errorf("Redacted = %v with no --redact", extract.Redacted)
	}

	for _, record := range extract.Records {
		if strings.Contains(record.Raw, "redacted:") {
			t.Error("a value was redacted without being asked for")
		}
	}
}

func TestHandoffRedaction(t *testing.T) {
	s := handoffFixture(t)

	extract := build(t, s, "level:error", HandoffOptions{Redact: []string{"user_id"}})

	tokens := map[string]string{}
	for i, record := range extract.Records {
		token, _ := record.Fields["user_id"].(string)
		if token == "" {
			continue
		}
		if !strings.HasPrefix(token, "redacted:") {
			t.Errorf("record %d: user_id = %q, want a redaction token", i, token)
		}

		// Leaving the value visible in the raw line while masking the field
		// would be a redaction that does not redact.
		if strings.Contains(record.Raw, "u_1") ||
			strings.Contains(record.Raw, "u_2") ||
			strings.Contains(record.Raw, "u_3") {
			t.Errorf("record %d: the original value survives in the raw line: %s", i, record.Raw)
		}
		tokens[record.Raw] = token
	}

	if len(tokens) == 0 {
		t.Fatal("nothing was redacted")
	}
}

// The token is stable, so records can still be correlated without the value
// being exposed.
func TestRedactionTokenIsStable(t *testing.T) {
	first := redactionToken("u_2")
	second := redactionToken("u_2")
	other := redactionToken("u_3")

	if first != second {
		t.Errorf("the same value hashed to %q then %q", first, second)
	}
	if first == other {
		t.Error("two different values hashed to the same token")
	}
	if strings.Contains(first, "u_2") {
		t.Errorf("the token leaks the value: %q", first)
	}
	if redactionToken("") != "" {
		t.Error("an empty value produced a token")
	}
}

// A field a record does not carry must not gain one.
func TestRedactionSkipsAbsentFields(t *testing.T) {
	s := handoffFixture(t)

	extract := build(t, s, "level:error", HandoffOptions{Redact: []string{"not_a_field"}})
	for _, record := range extract.Records {
		if _, ok := record.Fields["not_a_field"]; ok {
			t.Error("redacting an absent field invented it")
		}
	}
}

// Provenance: who produced the extract, with what, and when.
func TestHandoffProvenance(t *testing.T) {
	s := handoffFixture(t)

	extract := build(t, s, "", HandoffOptions{Version: "v1.2.3", Command: "loupe ./logs"})

	if extract.Meta.Tool != "loupe" {
		t.Errorf("Tool = %q", extract.Meta.Tool)
	}
	if extract.Meta.Version != "v1.2.3" {
		t.Errorf("Version = %q", extract.Meta.Version)
	}
	if extract.Meta.Command != "loupe ./logs" {
		t.Errorf("Command = %q", extract.Meta.Command)
	}
	if extract.Meta.At.IsZero() {
		t.Error("no timestamp on the extract")
	}
	if extract.Meta.Host == "" {
		t.Error("no hostname")
	}
}

// The resolver's assumptions travel with the extract, so a reader can see that
// a bare time was placed on a chosen day or that last: anchored to the data.
func TestHandoffCarriesResolutionNotes(t *testing.T) {
	s := handoffFixture(t)

	extract := build(t, s, "last:2m", HandoffOptions{})
	if len(extract.Notes) == 0 {
		t.Error("the last: anchor was not recorded in the extract")
	}
}

// A record whose timezone was assumed is marked, so the reader knows which
// timestamps depend on a flag.
func TestHandoffMarksAssumedTimezones(t *testing.T) {
	s := openFixture(t,
		`2026-08-13 14:00:00.000 [worker-1] ERROR c.a.p.Handler - read timed out`,
		`2026-08-13 14:01:00.000 [worker-1] ERROR c.a.p.Handler - again`,
	)

	extract := build(t, s, "", HandoffOptions{})

	if len(extract.Records) == 0 {
		t.Fatal("no records")
	}
	for i, record := range extract.Records {
		if !record.ZoneAssumed {
			t.Errorf("record %d: log4j carries no offset, so the zone is assumed", i)
		}
	}
	if len(extract.AssumedSources()) == 0 {
		t.Error("AssumedSources did not report the log4j source")
	}
}

func TestHandoffOnAnEmptyMatch(t *testing.T) {
	s := handoffFixture(t)

	extract := build(t, s, "status:>=999", HandoffOptions{})

	if extract.Counts.Matched != 0 {
		t.Errorf("Matched = %d, want 0", extract.Counts.Matched)
	}
	if len(extract.Records) != 0 {
		t.Errorf("got %d records for a filter matching nothing", len(extract.Records))
	}
	// The sources and counts still travel, so the reader can see what was
	// searched rather than receiving a bare "nothing".
	if len(extract.Sources) == 0 {
		t.Error("an empty extract dropped the source list")
	}
}
