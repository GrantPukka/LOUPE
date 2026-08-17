package parse

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// collect reads input with the named parser and returns every entry plus stats.
func collect(t *testing.T, parser, input string) ([]Entry, Stats) {
	t.Helper()
	p, ok := Get(parser)
	if !ok {
		t.Fatalf("parser %q not registered", parser)
	}

	var got []Entry
	stats, _, err := ReadAll(strings.NewReader(input), ReaderOptions{Parser: p}, func(e Entry) error {
		got = append(got, e)
		return nil
	})
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return got, stats
}

// The core principle: one malformed line must never abort a file, and must
// never disappear.
func TestMalformedLineDoesNotAbortTheFile(t *testing.T) {
	input := strings.Join([]string{
		`{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"first"}`,
		`{"ts":"2026-08-13T14:00:01Z","level":"info","msg":"trunca`, // truncated
		`not json at all`,
		``, // blank
		`2026-08-13 14:11:02 [notice] 1#1: signal 17 received`, // foreign format
		`{"ts":"2026-08-13T14:00:02Z","level":"info","msg":"last"}`,
	}, "\n")

	got, stats := collect(t, "jsonl", input)

	// Five non-blank lines in, five records out. Nothing dropped.
	if len(got) != 5 {
		t.Fatalf("got %d records, want 5:\n%s", len(got), dump(got))
	}
	if stats.Records != 5 {
		t.Errorf("stats.Records = %d, want 5", stats.Records)
	}
	if stats.Unparsed != 3 {
		t.Errorf("stats.Unparsed = %d, want 3", stats.Unparsed)
	}
	if stats.Blank != 1 {
		t.Errorf("stats.Blank = %d, want 1", stats.Blank)
	}

	// The record after the damage must still parse. A parser that gives up
	// after a bad line is the failure this test exists to catch.
	last := got[len(got)-1]
	if !last.Parsed || last.Message != "last" {
		t.Errorf("last record = %+v; reading did not recover after the damage", last.Record)
	}
}

// Raw text is always kept, including for lines nothing could parse. Handoffs
// include it because the receiver may not trust our parser.
func TestRawIsAlwaysKept(t *testing.T) {
	const junk = `!!! definitely not a log line !!!`
	got, _ := collect(t, "jsonl", junk)

	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	if got[0].Raw != junk {
		t.Errorf("Raw = %q, want %q", got[0].Raw, junk)
	}
	if got[0].Parsed {
		t.Error("Parsed = true for unparseable input")
	}
	// Unparsed does not mean unsearchable: the text has to be somewhere.
	if got[0].Message != junk {
		t.Errorf("Message = %q; an unparsed line must still be searchable", got[0].Message)
	}
}

// A record with no timestamp is still a record. It must be counted so a time
// filter can report what it excluded.
func TestRecordsWithoutTimestampsAreCounted(t *testing.T) {
	input := strings.Join([]string{
		`{"ts":"2026-08-13T14:00:00Z","msg":"has one"}`,
		`{"msg":"has none"}`,
		`{"msg":"also none"}`,
	}, "\n")

	got, stats := collect(t, "jsonl", input)

	if len(got) != 3 {
		t.Fatalf("got %d records, want 3", len(got))
	}
	if stats.NoTimestamp != 2 {
		t.Errorf("stats.NoTimestamp = %d, want 2", stats.NoTimestamp)
	}
	if got[0].HasTimestamp() == got[1].HasTimestamp() {
		t.Error("HasTimestamp does not distinguish the two cases")
	}
}

// A file whose last line has no trailing newline is what a killed process
// leaves behind. Dropping that line is silent data loss.
func TestFinalLineWithoutNewline(t *testing.T) {
	got, _ := collect(t, "jsonl", `{"msg":"first"}`+"\n"+`{"msg":"no trailing newline"}`)

	if len(got) != 2 {
		t.Fatalf("got %d records, want 2; the final line was dropped", len(got))
	}
	if got[1].Message != "no trailing newline" {
		t.Errorf("last message = %q", got[1].Message)
	}
}

func TestLineNumbersAreOneBasedAndCountBlanks(t *testing.T) {
	input := "line one\n\nline three\n"
	got, _ := collect(t, "text", input)

	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}
	if got[0].LineNo != 1 {
		t.Errorf("first LineNo = %d, want 1", got[0].LineNo)
	}
	// Blank lines still occupy a line number, or line numbers stop matching
	// what the user sees in their editor.
	if got[1].LineNo != 3 {
		t.Errorf("second LineNo = %d, want 3", got[1].LineNo)
	}
}

func TestStartLineOffsetsNumbering(t *testing.T) {
	p, _ := Get("text")
	var got []Entry
	_, _, err := ReadAll(strings.NewReader("a\nb\n"), ReaderOptions{Parser: p, StartLine: 100},
		func(e Entry) error { got = append(got, e); return nil })
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if got[0].LineNo != 101 {
		t.Errorf("LineNo = %d, want 101", got[0].LineNo)
	}
}

func TestCRLFIsStripped(t *testing.T) {
	got, _ := collect(t, "text", "windows line\r\nanother\r\n")

	for _, e := range got {
		if strings.HasSuffix(e.Raw, "\r") {
			t.Errorf("Raw = %q, carriage return not stripped", e.Raw)
		}
	}
}

// An overlong line is truncated rather than dropped or allowed to exhaust
// memory, and the fact is recorded.
func TestOverlongLineIsTruncatedNotDropped(t *testing.T) {
	huge := strings.Repeat("x", MaxLineBytes+5000)
	input := "short before\n" + huge + "\nshort after\n"

	got, stats := collect(t, "text", input)

	if len(got) != 3 {
		t.Fatalf("got %d records, want 3; the overlong line broke the read", len(got))
	}
	if !got[1].Truncated {
		t.Error("the overlong record is not marked Truncated")
	}
	if stats.Truncated != 1 {
		t.Errorf("stats.Truncated = %d, want 1", stats.Truncated)
	}
	if len(got[1].Raw) > MaxLineBytes {
		t.Errorf("Raw is %d bytes, longer than the cap", len(got[1].Raw))
	}
	// Reading must resume correctly on the next line rather than treating the
	// discarded remainder as a record.
	if got[2].Message != "short after" {
		t.Errorf("record after the overlong line = %q", got[2].Message)
	}
}

// A parser with no Continuer implementation must not fold lines together.
func TestNoContinuationWithoutAContinuer(t *testing.T) {
	got, stats := collect(t, "jsonl", "{\"msg\":\"a\"}\n\tindented follow-on\n")

	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}
	if stats.Continuation != 0 {
		t.Errorf("stats.Continuation = %d, want 0", stats.Continuation)
	}
}

// continuerParser is a stand-in for the log4j parser, which arrives in a later
// milestone. It proves the reader's folding logic independently of any format.
type continuerParser struct{ Parser }

func (continuerParser) Name() string { return "test-continuer" }

func (continuerParser) IsContinuation(line []byte) bool {
	return len(line) > 0 && (line[0] == '\t' || line[0] == ' ')
}

func (continuerParser) Parse(line []byte) (Record, error) {
	return Record{Message: string(line), Fields: map[string]any{}}, nil
}

func (continuerParser) Detect(sample [][]byte) float64 { return 0 }

func TestContinuationLinesFoldIntoTheRecordAbove(t *testing.T) {
	input := strings.Join([]string{
		"ERROR PaymentGatewayException: read timed out",
		"\tat com.acme.pay.GatewayClient.charge(GatewayClient.java:214)",
		"\tat com.acme.pay.Worker.consume(Worker.java:141)",
		"ERROR the next real record",
	}, "\n")

	var got []Entry
	stats, _, err := ReadAll(strings.NewReader(input),
		ReaderOptions{Parser: continuerParser{}},
		func(e Entry) error { got = append(got, e); return nil })
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d records, want 2:\n%s", len(got), dump(got))
	}
	if stats.Continuation != 2 {
		t.Errorf("stats.Continuation = %d, want 2", stats.Continuation)
	}
	if !strings.Contains(got[0].Message, "GatewayClient.java:214") {
		t.Error("stack trace not folded into the message")
	}
	// The raw text must reconstruct the original lines exactly.
	if strings.Count(got[0].Raw, "\n") != 2 {
		t.Errorf("Raw has %d newlines, want 2", strings.Count(got[0].Raw, "\n"))
	}
	if got[1].Message != "ERROR the next real record" {
		t.Errorf("second record = %q", got[1].Message)
	}
}

// A continuation line with nothing above it must not be swallowed.
func TestLeadingContinuationBecomesItsOwnRecord(t *testing.T) {
	var got []Entry
	_, _, err := ReadAll(strings.NewReader("\tan orphan continuation\nreal record\n"),
		ReaderOptions{Parser: continuerParser{}},
		func(e Entry) error { got = append(got, e); return nil })
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2; the orphan line was dropped", len(got))
	}
}

func TestReadAllRequiresAParser(t *testing.T) {
	_, _, err := ReadAll(strings.NewReader("x"), ReaderOptions{}, func(Entry) error { return nil })
	if err == nil {
		t.Fatal("expected an error when no parser is given")
	}
}

// The callback's error must stop the read and reach the caller, so an ingest
// failure is not mistaken for a clean run.
func TestCallbackErrorPropagates(t *testing.T) {
	p, _ := Get("text")
	want := fmt.Errorf("store is full")

	_, _, err := ReadAll(strings.NewReader("a\nb\nc\n"), ReaderOptions{Parser: p},
		func(Entry) error { return want })

	if err == nil {
		t.Fatal("callback error was swallowed")
	}
}

func TestAssumedTimezoneAppliesToZonelessTimestamps(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	// The same wall-clock text, read under two assumptions, is two different
	// instants nine hours apart. This is the trap in FILTER-DSL section 2.5.
	utc, _, ok := ParseTime("2026-08-13 14:00:00", time.UTC)
	if !ok {
		t.Fatal("failed to parse under UTC")
	}
	jst, zoned, ok := ParseTime("2026-08-13 14:00:00", tokyo)
	if !ok {
		t.Fatal("failed to parse under Asia/Tokyo")
	}
	if zoned {
		t.Error("a timestamp with no offset must not be reported as zoned")
	}
	if diff := utc.Sub(jst); diff != 9*time.Hour {
		t.Errorf("difference = %v, want 9h; the assumed zone was not applied", diff)
	}
}

func TestStatsDescribeMentionsEveryExclusion(t *testing.T) {
	s := Stats{Records: 1204, Unparsed: 3, NoTimestamp: 18, Truncated: 1, Continuation: 40}
	got := s.Describe()

	for _, want := range []string{"1204 records", "3 unparsed", "18 without a timestamp", "1 truncated"} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, missing %q", got, want)
		}
	}
}

func TestStatsAdd(t *testing.T) {
	var total Stats
	total.Add(Stats{Records: 10, Unparsed: 1, NoTimestamp: 2})
	total.Add(Stats{Records: 5, Unparsed: 3, Blank: 1})

	if total.Records != 15 || total.Unparsed != 4 || total.NoTimestamp != 2 || total.Blank != 1 {
		t.Errorf("total = %+v", total)
	}
}

func dump(entries []Entry) string {
	var sb strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&sb, "  line %d parsed=%v %q\n", e.LineNo, e.Parsed, e.Message)
	}
	return sb.String()
}

// The incremental-ingest contract. A file read while it is being written can
// stop mid-record: the classic case is a stack trace whose remaining frames
// have not been flushed yet. Resuming from the Tail must reproduce exactly the
// records a single read of the finished file would have produced — no
// duplicates, no orphaned continuation lines, no lost frames.
func TestTailResumesWithoutSplittingARecord(t *testing.T) {
	const head = "first record\n" +
		"second record\n" +
		"\tat com.example.Foo\n" +
		"\tat com.example.Bar\n"
	const grown = "\tat com.example.Baz\n" +
		"third record\n"

	whole := readWith(t, head+grown, ReaderOptions{})

	// Read 1: the file as it stood mid-write.
	partial := readWith(t, head, ReaderOptions{})
	_, tail, err := ReadAll(strings.NewReader(head),
		ReaderOptions{Parser: continuerParser{}}, func(Entry) error { return nil })
	if err != nil {
		t.Fatalf("first read: %v", err)
	}

	// The tail points at the start of the last record, so that record is read
	// again rather than being left half-formed.
	if tail.Line != 2 {
		t.Errorf("tail.Line = %d, want 2 (the record that was still being written)", tail.Line)
	}
	if int(tail.Offset) != len("first record\n") {
		t.Errorf("tail.Offset = %d, want %d", tail.Offset, len("first record\n"))
	}

	// Read 2: resume from the tail, as the store will, discarding the records
	// from tail.Line onward first.
	kept := partial[:tail.Line-1]
	resumed := readWith(t, head[tail.Offset:]+grown,
		ReaderOptions{StartLine: tail.Line - 1})

	got := append(append([]Entry{}, kept...), resumed...)

	if len(got) != len(whole) {
		t.Fatalf("resumed read produced %d records, single read produced %d", len(got), len(whole))
	}
	for i := range whole {
		if got[i].Raw != whole[i].Raw {
			t.Errorf("record %d raw =\n  %q\nwant\n  %q", i, got[i].Raw, whole[i].Raw)
		}
		if got[i].LineNo != whole[i].LineNo {
			t.Errorf("record %d line = %d, want %d", i, got[i].LineNo, whole[i].LineNo)
		}
	}
}

// readWith reads input with the continuation-aware test parser.
func readWith(t *testing.T, input string, opts ReaderOptions) []Entry {
	t.Helper()
	opts.Parser = continuerParser{}

	var got []Entry
	_, _, err := ReadAll(strings.NewReader(input), opts, func(e Entry) error {
		got = append(got, e)
		return nil
	})
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return got
}

// The status line's counts must survive an incremental read. Adding the
// resumed read's stats to Tail.Before has to total exactly what a single read
// of the finished file reports — the boundary record is read twice and must
// still be counted once.
func TestTailStatsTotalExactlyAcrossAResume(t *testing.T) {
	const head = "first record\n" +
		"\tcontinuation of first\n" +
		"\n" +
		"second record\n" +
		"\tat com.example.Foo\n"
	const grown = "\tat com.example.Bar\n" +
		"third record\n"

	var whole Stats
	whole, _, err := ReadAll(strings.NewReader(head+grown),
		ReaderOptions{Parser: continuerParser{}}, func(Entry) error { return nil })
	if err != nil {
		t.Fatalf("whole read: %v", err)
	}

	_, tail, err := ReadAll(strings.NewReader(head),
		ReaderOptions{Parser: continuerParser{}}, func(Entry) error { return nil })
	if err != nil {
		t.Fatalf("first read: %v", err)
	}

	resumed, _, err := ReadAll(strings.NewReader(head[tail.Offset:]+grown),
		ReaderOptions{Parser: continuerParser{}, StartLine: tail.Line - 1},
		func(Entry) error { return nil })
	if err != nil {
		t.Fatalf("resumed read: %v", err)
	}

	total := tail.Before
	total.Add(resumed)

	if total != whole {
		t.Errorf("incremental totals do not match a single read\n  incremental: %+v\n  single:      %+v",
			total, whole)
	}
}
