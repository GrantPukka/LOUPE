package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// traceFixture is three services and one that records no correlation id at
// all, which is the shape a real system has: the request went through Nginx
// too, and Nginx's combined format has nowhere to say so.
func traceFixture(t *testing.T) *Session {
	t.Helper()
	dir := t.TempDir()

	write := func(name string, lines ...string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	write("auth.log",
		`{"ts":"2026-08-13T14:00:00.000Z","level":"info","msg":"token validated","trace_id":"abc123"}`,
		`{"ts":"2026-08-13T14:00:10.000Z","level":"info","msg":"token validated","trace_id":"other"}`)

	write("api.log",
		`{"ts":"2026-08-13T14:00:00.500Z","level":"info","msg":"charging card","trace_id":"abc123"}`,
		`{"ts":"2026-08-13T14:00:04.500Z","level":"error","msg":"upstream timeout","trace_id":"abc123"}`)

	// A worker whose crash line lost its timestamp.
	write("worker.log",
		`{"ts":"2026-08-13T14:00:05.000Z","level":"error","msg":"charge failed","trace_id":"abc123"}`,
		`{"level":"fatal","msg":"worker died mid-write","trace_id":"abc123"}`)

	// Nginx: no correlation id anywhere in the format.
	write("access.log",
		`10.0.0.1 - - [13/Aug/2026:14:00:00 +0000] "POST /api/charge HTTP/1.1" 502 120 "-" "curl/8"`)

	sess, err := Open(context.Background(), Options{
		Paths:    []string{dir},
		Location: time.UTC,
		NoCache:  true,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func traceOf(t *testing.T, sess *Session, id string) Trace {
	t.Helper()

	got, err := sess.Trace(context.Background(), id, "")
	if err != nil {
		t.Fatalf("Trace(%q): %v", id, err)
	}
	return got
}

// The headline: one request's records, in the order they happened, across
// every source that saw it.
func TestTraceOrdersHopsAcrossSources(t *testing.T) {
	got := traceOf(t, traceFixture(t), "abc123")

	if len(got.Hops) != 5 {
		t.Fatalf("got %d hops, want 5: %+v", len(got.Hops), got.Hops)
	}

	want := []string{"token validated", "charging card", "upstream timeout", "charge failed"}
	for i, w := range want {
		if got.Hops[i].Message != w {
			t.Errorf("hop %d = %q, want %q", i, got.Hops[i].Message, w)
		}
	}

	// Only this trace, not the neighbouring one.
	for _, h := range got.Hops {
		if h.Message == "token validated" && h.Source != "auth" {
			t.Errorf("hop from the wrong source: %+v", h)
		}
	}
}

// The gap is the finding. A trace is usually lines that all look fine and one
// long wait between two of them.
func TestTraceMeasuresTheGapBetweenHops(t *testing.T) {
	got := traceOf(t, traceFixture(t), "abc123")

	// The first dated hop has nothing before it to measure from.
	if got.Hops[0].HasGap {
		t.Errorf("the first hop reported a gap of %s", got.Hops[0].Gap)
	}
	if want := 500 * time.Millisecond; got.Hops[1].Gap != want {
		t.Errorf("gap to the second hop = %s, want %s", got.Hops[1].Gap, want)
	}
	if want := 4 * time.Second; got.Hops[2].Gap != want {
		t.Errorf("gap to the third hop = %s, want %s", got.Hops[2].Gap, want)
	}

	if at := got.Slowest(); at != 2 {
		t.Errorf("slowest hop = %d, want 2 (the four-second wait)", at)
	}
	if want := 5 * time.Second; got.Span != want {
		t.Errorf("span = %s, want %s", got.Span, want)
	}
}

// A record with no clock is still a record, and on a crashed service it is
// often the last one. It is ordered last and counted, never dropped.
func TestTraceKeepsUndatedHopsAndSaysSo(t *testing.T) {
	got := traceOf(t, traceFixture(t), "abc123")

	if got.Undated != 1 {
		t.Fatalf("undated = %d, want 1", got.Undated)
	}

	last := got.Hops[len(got.Hops)-1]
	if last.Dated() {
		t.Errorf("the undated hop is not last: %+v", got.Hops)
	}
	if last.Message != "worker died mid-write" {
		t.Errorf("last hop = %q, want the undated one", last.Message)
	}
	// It has no gap, because there is nothing to measure.
	if last.HasGap {
		t.Errorf("an undated hop reported a gap of %s", last.Gap)
	}
}

// The distinction the whole view turns on: a source that records correlation
// ids and has none for this trace probably did not handle the request; one
// that never records them may have and cannot say.
func TestTraceSeparatesSilentSourcesFromBlindOnes(t *testing.T) {
	got := traceOf(t, traceFixture(t), "abc123")

	present := names(got.Present())
	if len(present) != 3 {
		t.Errorf("present = %v, want the three services that saw it", present)
	}

	blind := names(got.Blind())
	if len(blind) != 1 || blind[0] != "access" {
		t.Errorf("blind = %v, want [access] — it records no correlation id", blind)
	}

	if silent := names(got.Silent()); len(silent) != 0 {
		t.Errorf("silent = %v, want none for a trace every service saw", silent)
	}
}

// A trace only one service saw must mark the others silent, not blind: they
// could have said and did not.
func TestTraceMarksSourcesThatCouldHaveSaidAndDidNot(t *testing.T) {
	got := traceOf(t, traceFixture(t), "other")

	if len(got.Hops) != 1 {
		t.Fatalf("got %d hops, want 1", len(got.Hops))
	}

	silent := names(got.Silent())
	if len(silent) != 2 {
		t.Errorf("silent = %v, want the two services that record ids but not this one", silent)
	}
	if blind := names(got.Blind()); len(blind) != 1 || blind[0] != "access" {
		t.Errorf("blind = %v, want [access]", blind)
	}
}

// An id nothing carries is a real answer, and it still has to say which
// sources could not have been asked.
func TestTraceNotFoundStillReportsReach(t *testing.T) {
	got := traceOf(t, traceFixture(t), "nosuchtrace")

	if got.Found() {
		t.Fatalf("a made-up id matched %d hops", len(got.Hops))
	}
	if len(got.Blind()) != 1 {
		t.Errorf("blind = %v, want the source that records no ids", names(got.Blind()))
	}
	if len(got.Silent()) != 3 {
		t.Errorf("silent = %v, want the three that do", names(got.Silent()))
	}
}

// Detection beats a flag. The field chosen is reported, because an assumption
// nobody can see is one nobody checks.
func TestDetectTraceField(t *testing.T) {
	sess := traceFixture(t)

	got, err := sess.DetectTraceField(context.Background())
	if err != nil {
		t.Fatalf("DetectTraceField: %v", err)
	}
	if got.Name != "trace_id" {
		t.Errorf("detected %q, want trace_id", got.Name)
	}
	if got.Records == 0 {
		t.Error("detected field reports no records")
	}
}

// Coverage decides, not the order of the candidate list. A request_id on three
// records must not outrank a trace_id on three hundred.
func TestDetectTraceFieldPrefersTheBetterCoveredCandidate(t *testing.T) {
	dir := t.TempDir()

	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines,
			`{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"a","correlation_id":"c1"}`)
	}
	// One record carrying the higher-priority name, and twenty carrying the
	// lower-priority one.
	lines = append(lines,
		`{"ts":"2026-08-13T14:00:01Z","level":"info","msg":"b","trace_id":"t1"}`)

	if err := os.WriteFile(filepath.Join(dir, "app.log"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	sess, err := Open(context.Background(), Options{
		Paths: []string{dir}, Location: time.UTC, NoCache: true,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close()

	got, err := sess.DetectTraceField(context.Background())
	if err != nil {
		t.Fatalf("DetectTraceField: %v", err)
	}
	if got.Name != "correlation_id" {
		t.Errorf("detected %q, want correlation_id — it covers twenty records to trace_id's one",
			got.Name)
	}
	// The runner-up is named, so a wrong guess is visible.
	if len(got.Others) == 0 {
		t.Error("the other candidate present was not reported")
	}
}

// Data with nothing correlation-shaped says so, and says what it looked for.
func TestDetectTraceFieldOnDataWithNone(t *testing.T) {
	sess := openFixture(t,
		`{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"no ids here"}`)

	_, err := sess.DetectTraceField(context.Background())
	if err == nil {
		t.Fatal("detection succeeded on data with no correlation field")
	}
	if !strings.Contains(err.Error(), "trace_id") {
		t.Errorf("error does not say what was looked for: %v", err)
	}
	if !strings.Contains(err.Error(), "--field") {
		t.Errorf("error does not offer the way out: %v", err)
	}
}

// An explicit field is followed even when detection would have chosen another.
func TestTraceHonoursAnExplicitField(t *testing.T) {
	sess := traceFixture(t)

	got, err := sess.Trace(context.Background(), "abc123", "trace_id")
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if got.Field != "trace_id" {
		t.Errorf("field = %q, want the one asked for", got.Field)
	}

	if _, err := sess.Trace(context.Background(), "abc123", "nosuchfield"); err == nil {
		t.Error("an unknown --field was accepted silently")
	}
}

// An id with a quote or a space in it must still produce a filter that parses
// back to what was meant, because the term is built from the AST rather than
// pasted together.
func TestTraceHandlesAwkwardIDs(t *testing.T) {
	sess := traceFixture(t)

	for _, id := range []string{`a"b`, "a b", "a:b", ""} {
		got, err := sess.Trace(context.Background(), id, "trace_id")
		if err != nil {
			t.Errorf("Trace(%q): %v", id, err)
			continue
		}
		if got.Found() {
			t.Errorf("Trace(%q) matched %d hops, want none", id, len(got.Hops))
		}
	}
}

func names(reach []SourceReach) []string {
	out := make([]string, len(reach))
	for i, r := range reach {
		out[i] = r.Name
	}
	return out
}

// An extract about one request carries the timeline, not just the records.
func TestTraceHandoffCarriesTheTimeline(t *testing.T) {
	sess := traceFixture(t)
	trace := traceOf(t, sess, "abc123")

	got, err := sess.TraceHandoff(context.Background(), trace, HandoffOptions{})
	if err != nil {
		t.Fatalf("TraceHandoff: %v", err)
	}

	if got.Trace == nil {
		t.Fatal("the extract has no trace section")
	}
	if got.Trace.ID != "abc123" || got.Trace.Field != "trace_id" {
		t.Errorf("trace section = %+v, want the id and field followed", got.Trace)
	}
	if len(got.Trace.Hops) != len(trace.Hops) {
		t.Errorf("extract has %d hops, the trace had %d", len(got.Trace.Hops), len(trace.Hops))
	}

	// The record table has to agree with the timeline above it, or the extract
	// contradicts itself.
	if got.Counts.Matched != int64(len(trace.Hops)) {
		t.Errorf("matched %d records but the timeline shows %d hops",
			got.Counts.Matched, len(trace.Hops))
	}
}

// Both times on every hop, so neither end of the handoff does offset
// arithmetic.
func TestTraceHandoffHopsCarryBothZones(t *testing.T) {
	sess := traceFixture(t)
	got, err := sess.TraceHandoff(context.Background(), traceOf(t, sess, "abc123"), HandoffOptions{})
	if err != nil {
		t.Fatalf("TraceHandoff: %v", err)
	}

	var dated, undated int
	for _, h := range got.Trace.Hops {
		if h.Local == "" {
			undated++
			if h.UTC != "" {
				t.Errorf("an undated hop carries a UTC time: %+v", h)
			}
			continue
		}
		dated++
		if h.UTC == "" {
			t.Errorf("hop %q has a local time but no UTC one", h.Message)
		}
	}
	if dated == 0 {
		t.Error("no hop carried a time")
	}
	if undated != 1 {
		t.Errorf("%d undated hops in the extract, want 1", undated)
	}
}

// The largest wait is marked, because it is usually the finding.
func TestTraceHandoffMarksTheSlowestHop(t *testing.T) {
	sess := traceFixture(t)
	got, err := sess.TraceHandoff(context.Background(), traceOf(t, sess, "abc123"), HandoffOptions{})
	if err != nil {
		t.Fatalf("TraceHandoff: %v", err)
	}

	marked := 0
	for _, h := range got.Trace.Hops {
		if h.Slowest {
			marked++
			if h.Message != "upstream timeout" {
				t.Errorf("marked %q as slowest, want the four-second wait", h.Message)
			}
		}
	}
	if marked != 1 {
		t.Errorf("%d hops marked slowest, want exactly 1", marked)
	}
}

// A receiver cannot see the absence of a source that was never able to answer,
// so the extract has to say which those were.
func TestTraceHandoffCarriesWhatCouldNotBeChecked(t *testing.T) {
	sess := traceFixture(t)

	got, err := sess.TraceHandoff(context.Background(), traceOf(t, sess, "abc123"), HandoffOptions{})
	if err != nil {
		t.Fatalf("TraceHandoff: %v", err)
	}
	if len(got.Trace.Blind) != 1 || got.Trace.Blind[0] != "access" {
		t.Errorf("blind = %v, want [access]", got.Trace.Blind)
	}
	if got.Trace.Undated != 1 {
		t.Errorf("undated = %d, want 1", got.Trace.Undated)
	}

	// And for a trace only one service saw, the silent ones travel too.
	other, err := sess.TraceHandoff(context.Background(), traceOf(t, sess, "other"), HandoffOptions{})
	if err != nil {
		t.Fatalf("TraceHandoff: %v", err)
	}
	if len(other.Trace.Silent) != 2 {
		t.Errorf("silent = %v, want the two services that record ids but not this one",
			other.Trace.Silent)
	}
}

// An extract that is not about a trace must not grow a trace section.
func TestOrdinaryHandoffHasNoTraceSection(t *testing.T) {
	sess := traceFixture(t)

	got, err := sess.Handoff(context.Background(), plan(t, sess, "level:error"), HandoffOptions{})
	if err != nil {
		t.Fatalf("Handoff: %v", err)
	}
	if got.Trace != nil {
		t.Errorf("a filter extract carries a trace section: %+v", got.Trace)
	}
}

// correlationFixture is the shape that used to defeat detection: a trace_id on
// almost every record, and the id the user actually pasted sitting in a
// correlation_id on one — plus lines that mention it only in their text.
func correlationFixture(t *testing.T) *Session {
	t.Helper()
	dir := t.TempDir()

	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, `{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"handled","trace_id":"t`+
			strings.Repeat("0", 3)+string(rune('a'+i%26))+`"}`)
	}
	lines = append(lines,
		`{"ts":"2026-08-13T14:01:00Z","level":"error","msg":"payment declined","correlation_id":"req-7f3c"}`,
		`2026-08-13 14:01:01 ERROR gateway upstream failed for req-7f3c retrying`,
		`2026-08-13 14:01:02 ERROR gateway giving up on req-7f3c`,
	)

	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	sess, err := Open(context.Background(), Options{
		Paths:    []string{dir},
		Location: time.UTC,
		NoCache:  true,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

// Detection used to pick the best-covered field and then report that no record
// carried the id — a confidently wrong answer to a question it had all the
// information to answer.
func TestTraceFieldPrefersTheFieldHoldingTheID(t *testing.T) {
	sess := correlationFixture(t)
	ctx := context.Background()

	// Knowing nothing about the id, coverage is the only signal there is.
	byCoverage, err := sess.DetectTraceField(ctx)
	if err != nil {
		t.Fatalf("DetectTraceField: %v", err)
	}
	if byCoverage.Name != "trace_id" {
		t.Errorf("without an id, coverage should win: got %q", byCoverage.Name)
	}

	// Given the id, the field that holds it wins.
	byValue, err := sess.DetectTraceFieldFor(ctx, "req-7f3c")
	if err != nil {
		t.Fatalf("DetectTraceFieldFor: %v", err)
	}
	if byValue.Name != "correlation_id" {
		t.Errorf("field = %q, want correlation_id — it is the one holding the id", byValue.Name)
	}
}

// A trace has to include the records that mention the id without carrying it as
// a field, or it shows one hop of six and reads as though the rest never
// happened.
func TestTraceIncludesTextOnlyHops(t *testing.T) {
	sess := correlationFixture(t)

	tr, err := sess.Trace(context.Background(), "req-7f3c", "")
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}

	if len(tr.Hops) != 3 {
		t.Fatalf("hops = %d, want 3 (one field match and two text matches)", len(tr.Hops))
	}
	if tr.TextOnly != 2 {
		t.Errorf("TextOnly = %d, want 2 — the caveat has to be countable", tr.TextOnly)
	}

	marked := 0
	for _, h := range tr.Hops {
		if h.TextOnly {
			marked++
		}
	}
	if marked != 2 {
		t.Errorf("%d hops marked TextOnly, want 2", marked)
	}
}

// A record that carries the field is never double-counted as a text match, even
// though its raw line contains the id too.
func TestTraceDoesNotDoubleCountFieldHops(t *testing.T) {
	sess := traceFixture(t)

	tr, err := sess.Trace(context.Background(), "abc123", "")
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}

	seen := map[int64]bool{}
	for _, h := range tr.Hops {
		if seen[h.Seq] {
			t.Fatalf("hop seq %d appears twice", h.Seq)
		}
		seen[h.Seq] = true
	}
	if tr.TextOnly != 0 {
		t.Errorf("TextOnly = %d, want 0 — every one of these carries the field", tr.TextOnly)
	}
}
