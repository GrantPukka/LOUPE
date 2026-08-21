package parse

import (
	"strings"
	"testing"
	"time"
)

// parseWith runs one line through a named parser.
func parseWith(t *testing.T, name, line string) Record {
	t.Helper()

	p, ok := Get(name)
	if !ok {
		t.Fatalf("no parser named %q", name)
	}
	rec, err := p.Parse([]byte(line))
	if err != nil {
		t.Fatalf("%s.Parse(%q): %v", name, line, err)
	}
	return rec
}

func refuses(t *testing.T, name, line string) {
	t.Helper()

	p, ok := Get(name)
	if !ok {
		t.Fatalf("no parser named %q", name)
	}
	if _, err := p.Parse([]byte(line)); err != ErrNoMatch {
		t.Errorf("%s claimed %q (err = %v)", name, line, err)
	}
}

// journald writes its timestamp as microseconds since the epoch in a string,
// because the value needs more bits than a JSON double holds exactly.
func TestJournaldTimestamp(t *testing.T) {
	rec := parseWith(t, "journald",
		`{"__REALTIME_TIMESTAMP":"1786629720123456","MESSAGE":"hello","PRIORITY":"6"}`)

	want := time.Date(2026, 8, 13, 14, 2, 0, 123456000, time.UTC)
	if !rec.Timestamp.Equal(want) {
		t.Errorf("ts = %s, want %s", rec.Timestamp, want)
	}
	// An epoch instant carries its own zone by construction, so nothing was
	// assumed and nothing needs disclosing.
	if !rec.TimestampZoned {
		t.Error("an epoch timestamp was reported as needing an assumed zone")
	}
}

// PRIORITY is a syslog severity digit, mapped by the same function the syslog
// parser uses — a second mapping here could disagree with that one.
func TestJournaldPriorityMapsToLevel(t *testing.T) {
	tests := map[string]string{
		"0": "fatal", "1": "fatal", "2": "fatal",
		"3": "error", "4": "warn", "5": "info", "6": "info", "7": "debug",
	}

	for priority, want := range tests {
		rec := parseWith(t, "journald",
			`{"__REALTIME_TIMESTAMP":"1786629720000000","MESSAGE":"m","PRIORITY":"`+priority+`"}`)
		if rec.Level != want {
			t.Errorf("PRIORITY %s = %q, want %q", priority, rec.Level, want)
		}
		// The digit is kept as well as the word: it is what a systemd user
		// filters on.
		if rec.Fields["priority"] != int64(mustAtoi(t, priority)) {
			t.Errorf("PRIORITY %s did not keep its numeric form: %v", priority, rec.Fields["priority"])
		}
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	if len(s) != 1 || s[0] < '0' || s[0] > '9' {
		t.Fatalf("not a digit: %q", s)
	}
	return int(s[0] - '0')
}

// A PRIORITY that is not a digit is kept as an ordinary field rather than
// dropped. Anything dropped here is invisible forever.
func TestJournaldKeepsAnUnreadablePriority(t *testing.T) {
	rec := parseWith(t, "journald",
		`{"__REALTIME_TIMESTAMP":"1786629720000000","MESSAGE":"m","PRIORITY":"notice"}`)

	if rec.Level != "" {
		t.Errorf("level = %q, want empty for an unreadable priority", rec.Level)
	}
	if rec.Fields["PRIORITY"] != "notice" {
		t.Errorf("PRIORITY was dropped: %v", rec.Fields)
	}
}

// systemd exports a message as an array of byte values when it holds bytes JSON
// cannot carry. Those are exactly the lines somebody is hunting for.
func TestJournaldByteArrayMessage(t *testing.T) {
	rec := parseWith(t, "journald",
		`{"__REALTIME_TIMESTAMP":"1786629720000000","MESSAGE":[104,105,0,116,104,101,114,101]}`)

	if want := "hi\x00there"; rec.Message != want {
		t.Errorf("message = %q, want %q", rec.Message, want)
	}
}

// A journal entry with no realtime timestamp is still a record. ts:none finds
// it; refusing it would lose the line.
func TestJournaldWithoutATimestamp(t *testing.T) {
	rec := parseWith(t, "journald", `{"__CURSOR":"s=1;i=2","MESSAGE":"no clock","PRIORITY":"6"}`)

	if rec.HasTimestamp() {
		t.Errorf("ts = %s, want none", rec.Timestamp)
	}
	if rec.Message != "no clock" {
		t.Errorf("message = %q", rec.Message)
	}
}

// Valid JSON without journal metadata is not a journal entry, and claiming it
// would mangle every JSON log in a mixed directory.
func TestJournaldRefusesOrdinaryJSON(t *testing.T) {
	refuses(t, "journald", `{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"a"}`)
	refuses(t, "journald", `not json`)
}

// Docker's driver appends the newline the process wrote. Keeping it would put a
// blank line under every record.
func TestDockerTrimsTheTrailingNewline(t *testing.T) {
	rec := parseWith(t, "docker",
		`{"log":"listening on :8080\n","stream":"stdout","time":"2026-08-13T14:02:00.113456789Z"}`)

	if rec.Message != "listening on :8080" {
		t.Errorf("message = %q, want it without the trailing newline", rec.Message)
	}
	if rec.Fields["stream"] != "stdout" {
		t.Errorf("stream = %v, want stdout", rec.Fields["stream"])
	}
	want := time.Date(2026, 8, 13, 14, 2, 0, 113456789, time.UTC)
	if !rec.Timestamp.Equal(want) {
		t.Errorf("ts = %s, want %s", rec.Timestamp, want)
	}
}

// An empty payload is a real line the container wrote, not an absent one.
func TestDockerKeepsAnEmptyMessage(t *testing.T) {
	rec := parseWith(t, "docker",
		`{"log":"\n","stream":"stdout","time":"2026-08-13T14:02:03Z"}`)

	if rec.Message != "" {
		t.Errorf("message = %q, want empty", rec.Message)
	}
	if !rec.HasTimestamp() {
		t.Error("an empty message lost its timestamp")
	}
}

// A malformed time is not fatal: the record is kept and ts:none finds it.
func TestDockerSurvivesAnUnreadableTime(t *testing.T) {
	rec := parseWith(t, "docker", `{"log":"m\n","stream":"stdout","time":"not-a-timestamp"}`)

	if rec.HasTimestamp() {
		t.Errorf("ts = %s, want none", rec.Timestamp)
	}
	if rec.Message != "m" {
		t.Errorf("message = %q", rec.Message)
	}
}

// The trio of keys is the signature. Two of them are names any application log
// might use, so a line missing one is not a docker line.
func TestDockerNeedsAllThreeKeys(t *testing.T) {
	refuses(t, "docker", `{"log":"m\n","time":"2026-08-13T14:02:00Z"}`)
	refuses(t, "docker", `{"stream":"stdout","time":"2026-08-13T14:02:00Z"}`)
	refuses(t, "docker", `{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"a"}`)
}

// Labels from --log-opt are flattened so a filter can name one directly.
func TestDockerFlattensAttrs(t *testing.T) {
	rec := parseWith(t, "docker",
		`{"log":"m\n","stream":"stderr","time":"2026-08-13T14:02:00Z","attrs":{"tag":"checkout-api"}}`)

	if rec.Fields["tag"] != "checkout-api" {
		t.Errorf("attrs were not flattened: %v", rec.Fields)
	}
}

// Neither container format carries a severity, so it is read out of the message
// text rather than guessed from the stream.
func TestContainerLevelComesFromTheMessageNotTheStream(t *testing.T) {
	tests := []struct{ name, line, want string }{
		{"docker", `{"log":"ERROR read timed out\n","stream":"stdout","time":"2026-08-13T14:02:00Z"}`, "error"},
		{"docker", `{"log":"listening on :8080\n","stream":"stderr","time":"2026-08-13T14:02:00Z"}`, ""},
		{"cri", `2026-08-13T14:02:00Z stdout F WARN retrying`, "warn"},
		{"cri", `2026-08-13T14:02:00Z stderr F listening on :8080`, ""},
	}

	for _, tc := range tests {
		rec := parseWith(t, tc.name, tc.line)
		if rec.Level != tc.want {
			t.Errorf("%s %q: level = %q, want %q", tc.name, tc.line, rec.Level, tc.want)
		}
	}
}

func TestCRIFields(t *testing.T) {
	rec := parseWith(t, "cri", `2026-08-13T14:02:00.113456789Z stderr F read timed out after 5000ms`)

	want := time.Date(2026, 8, 13, 14, 2, 0, 113456789, time.UTC)
	if !rec.Timestamp.Equal(want) {
		t.Errorf("ts = %s, want %s", rec.Timestamp, want)
	}
	if rec.Fields["stream"] != "stderr" {
		t.Errorf("stream = %v", rec.Fields["stream"])
	}
	if rec.Message != "read timed out after 5000ms" {
		t.Errorf("message = %q", rec.Message)
	}
	if _, marked := rec.Fields["partial"]; marked {
		t.Error("a full line was marked partial")
	}
}

// A P-tagged line is a fragment the runtime split. It is kept as its own record
// and marked, rather than stitched: joining needs to know the previous line's
// tag, and the Continuer interface asks the opposite question.
func TestCRIMarksPartialLines(t *testing.T) {
	rec := parseWith(t, "cri", `2026-08-13T14:02:02Z stdout P a very long line that was cut`)

	if rec.Fields["partial"] != true {
		t.Errorf("a P-tagged line was not marked partial: %v", rec.Fields)
	}
	if rec.Message != "a very long line that was cut" {
		t.Errorf("message = %q", rec.Message)
	}
}

// A line with no message at all is still four fields' worth of record.
func TestCRIWithNoMessage(t *testing.T) {
	rec := parseWith(t, "cri", `2026-08-13T14:02:03Z stdout F`)

	if rec.Message != "" {
		t.Errorf("message = %q, want empty", rec.Message)
	}
	if !rec.HasTimestamp() {
		t.Error("a blank line lost its timestamp")
	}
}

func TestCRIRefusesOtherShapes(t *testing.T) {
	for _, line := range []string{
		`2026-08-13T14:02:00Z stdxxx F unknown stream`,
		`2026-08-13T14:02:00Z stdout X unknown tag`,
		`not-a-time stdout F bad timestamp`,
		`2026-08-13T14:02:00Z stdout`,
		`plain text`,
		``,
	} {
		refuses(t, "cri", line)
	}
}

// The spec reserves colon-separated extensions on the tag. Accepting them costs
// two lines and refusing them would drop every record the day one appears.
func TestCRIAcceptsTagExtensions(t *testing.T) {
	rec := parseWith(t, "cri", `2026-08-13T14:02:00Z stdout F:something reserved`)

	if rec.Message != "reserved" {
		t.Errorf("message = %q", rec.Message)
	}
}

// The generic JSON parser must lose to a specific one. Detection ties broken by
// alphabetical order would be an accident waiting to be renamed.
func TestSpecificJSONParsersBeatGenericJSON(t *testing.T) {
	tests := map[string]string{
		"journald": `{"__REALTIME_TIMESTAMP":"1786629720000000","PRIORITY":"6","MESSAGE":"m","_HOSTNAME":"h"}`,
		"docker":   `{"log":"m\n","stream":"stdout","time":"2026-08-13T14:02:00Z"}`,
	}

	for want, line := range tests {
		t.Run(want, func(t *testing.T) {
			sample := [][]byte{[]byte(line), []byte(line), []byte(line)}

			got := Detect(sample)
			if got.Parser.Name() != want {
				t.Fatalf("detected %s (%.2f), want %s", got.Parser.Name(), got.Confidence, want)
			}
			if got.Ambiguous() {
				t.Errorf("%s beat %s by only %.2f — a rename would flip it",
					want, got.Runner.Name(), got.Confidence-got.RunnerUp)
			}
		})
	}
}

// And generic JSON still wins over everything that is not JSON at all.
func TestGenericJSONStillWinsWhenNothingIsMoreSpecific(t *testing.T) {
	line := `{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"a","status":200}`
	sample := [][]byte{[]byte(line), []byte(line), []byte(line)}

	got := Detect(sample)
	if got.Parser.Name() != "jsonl" {
		t.Fatalf("detected %s, want jsonl", got.Parser.Name())
	}
	if got.Ambiguous() {
		t.Errorf("plain jsonl reported ambiguous against %s (%.2f vs %.2f)",
			got.Runner.Name(), got.Confidence, got.RunnerUp)
	}
}

// A mixed container directory must not have its formats crossed: a CRI line
// reaching the docker parser, or the reverse, would mangle both.
func TestContainerFormatsDoNotCrossClaim(t *testing.T) {
	docker := `{"log":"m\n","stream":"stdout","time":"2026-08-13T14:02:00Z"}`
	cri := `2026-08-13T14:02:00Z stdout F m`

	refuses(t, "cri", docker)
	refuses(t, "docker", cri)

	if strings.Contains(parseWith(t, "jsonl", docker).Message, `"stream"`) {
		t.Log("jsonl reads a docker line as generic JSON, which is why docker outranks it")
	}
}
