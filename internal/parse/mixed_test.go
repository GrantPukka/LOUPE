package parse

import (
	"errors"
	"strings"
	"testing"
)

// One merged file, six formats, as journalctl or `cat *.log > combined.log`
// produces. Every line here must reach the timeline under its own format.
func TestMixedParsesEachLineInItsOwnFormat(t *testing.T) {
	tests := []struct {
		name       string
		line       string
		wantFormat string
		wantMsg    string
	}{
		{
			name:       "jsonl",
			line:       `{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"request completed","status":200}`,
			wantFormat: "jsonl",
			wantMsg:    "request completed",
		},
		{
			name:       "logfmt",
			line:       `ts=2026-08-13T14:00:01Z level=error msg="upstream timeout" service=checkout`,
			wantFormat: "logfmt",
			wantMsg:    "upstream timeout",
		},
		{
			name:       "nginx",
			line:       `10.0.3.48 - - [13/Aug/2026:14:00:02 +0000] "GET /healthz HTTP/1.1" 200 512 "-" "curl/8.4.0"`,
			wantFormat: "nginx",
		},
		{
			name:       "syslog",
			line:       `<34>1 2026-08-13T14:00:03Z host auth-svc 1234 ID47 - session opened for user deploy`,
			wantFormat: "syslog",
		},
	}

	mixed, ok := Get(MixedName)
	if !ok {
		t.Fatal("the mixed parser is not registered")
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, err := mixed.Parse([]byte(tt.line))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if rec.Format != tt.wantFormat {
				t.Errorf("Format = %q, want %q", rec.Format, tt.wantFormat)
			}
			if tt.wantMsg != "" && rec.Message != tt.wantMsg {
				t.Errorf("Message = %q, want %q", rec.Message, tt.wantMsg)
			}
			if !rec.HasTimestamp() {
				t.Error("no timestamp: the record would be off the timeline")
			}
		})
	}
}

// A line no real format claims must be unparsed, not quietly swept up by the
// fallback. The unparsed count is what makes this tool's limits visible, and a
// mode that drove it to zero would be lying about how much it understood.
func TestMixedLeavesUnknownLinesUnparsed(t *testing.T) {
	mixed, _ := Get(MixedName)

	for _, line := range []string{
		"just some prose nobody logged in a format",
		"\x00\x01 binary garbage",
	} {
		if _, err := mixed.Parse([]byte(line)); !errors.Is(err, ErrNoMatch) {
			t.Errorf("Parse(%q) err = %v, want ErrNoMatch", line, err)
		}
	}
}

// The same line always parses the same way, wherever it appears.
func TestMixedIsDeterministic(t *testing.T) {
	mixed, _ := Get(MixedName)
	line := []byte(`ts=2026-08-13T14:00:01Z level=error msg="upstream timeout"`)

	first, err := mixed.Parse(line)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := mixed.Parse(line)
		if err != nil || again.Format != first.Format {
			t.Fatalf("run %d: format %q err %v, want %q", i, again.Format, err, first.Format)
		}
	}
}

// A Java stack trace is one record. Per-line detection must not trade
// file-level fragmentation for line-level fragmentation.
func TestMixedFoldsContinuations(t *testing.T) {
	mixed, _ := Get(MixedName)
	c, ok := mixed.(Continuer)
	if !ok {
		t.Fatal("the mixed parser should support continuations")
	}

	if !c.IsContinuation([]byte("\tat com.example.Service.handle(Service.java:42)")) {
		t.Error("a Java stack frame should continue the preceding record")
	}
	if c.IsContinuation([]byte(`{"ts":"2026-08-13T14:00:00Z","msg":"a"}`)) {
		t.Error("a complete JSON record starts a new record")
	}
}

// A multi-line format must not be judged on its continuation lines.
//
// A Log4j file full of Java stack traces is mostly `at com.example…` frames,
// none of which parse alone and none of which are the parser's failure — they
// belong to the record above. Counting them scored Log4j at well under half on
// its own files, sent every one of them to per-line detection, mislabelled the
// format, broke the stack traces back into separate records, and made ingest
// half as fast again.
func TestCoverageIgnoresContinuationLines(t *testing.T) {
	log4j, ok := Get("log4j")
	if !ok {
		t.Fatal("log4j is not registered")
	}

	sample := [][]byte{
		[]byte(`2026-08-13 14:00:00,123 ERROR [main] com.acme.pay.Gateway - read timed out`),
		[]byte("\tat com.acme.pay.Gateway.send(Gateway.java:88)"),
		[]byte("\tat com.acme.pay.Worker.run(Worker.java:41)"),
		[]byte("Caused by: java.net.SocketTimeoutException"),
		[]byte("\tat java.base/java.net.Socket.connect(Socket.java:615)"),
	}

	if got := Coverage(log4j, sample); got != 1 {
		t.Errorf("Coverage = %v, want 1 — four of these five lines are one record", got)
	}
}

func TestCoverage(t *testing.T) {
	jsonl, _ := Get("jsonl")

	sample := [][]byte{
		[]byte(`{"ts":"2026-08-13T14:00:00Z","msg":"a"}`),
		[]byte(`{"ts":"2026-08-13T14:00:01Z","msg":"b"}`),
		[]byte(`10.0.3.48 - - [13/Aug/2026:14:00:02 +0000] "GET /healthz HTTP/1.1" 200 512 "-" "-"`),
		[]byte(`ts=2026-08-13T14:00:03Z level=info msg=hello`),
	}

	if got := Coverage(jsonl, sample); got != 0.5 {
		t.Errorf("Coverage = %v, want 0.5 — jsonl reads two of these four lines", got)
	}
	if got := Coverage(jsonl, nil); got != 1 {
		t.Errorf("Coverage of an empty sample = %v, want 1", got)
	}
}

func TestFormats(t *testing.T) {
	got := Formats([][]byte{
		[]byte(`{"ts":"2026-08-13T14:00:00Z","msg":"a"}`),
		[]byte(`ts=2026-08-13T14:00:03Z level=info msg=hello`),
		[]byte(`not a log line in any format at all`),
	})

	if got["jsonl"] != 1 || got["logfmt"] != 1 {
		t.Errorf("Formats = %v, want one jsonl and one logfmt", got)
	}
	if got[UnclaimedFormat] != 1 {
		t.Errorf("Formats = %v, want one unclaimed line — the totals must add up", got)
	}
}

// The mixed parser must never be picked by ordinary detection. Choosing it is a
// judgement about coverage, which a confidence score cannot express.
func TestMixedNeverWinsDetection(t *testing.T) {
	sample := [][]byte{
		[]byte(`{"ts":"2026-08-13T14:00:00Z","msg":"a"}`),
		[]byte(`ts=2026-08-13T14:00:03Z level=info msg=hello`),
	}

	if det := Detect(sample); det.Parser != nil && det.Parser.Name() == MixedName {
		t.Error("Detect chose the mixed parser")
	}
}

// It is still selectable by name, so --parser mixed works.
func TestMixedIsSelectableByName(t *testing.T) {
	if _, ok := Get(MixedName); !ok {
		t.Fatalf("--parser %s would not resolve", MixedName)
	}
	if !strings.Contains(strings.Join(Names(), ","), MixedName) {
		t.Errorf("%s is missing from the parser list users are shown: %v", MixedName, Names())
	}
}
