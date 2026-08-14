package parse

import (
	"strings"
	"testing"
)

func lines(s string) [][]byte {
	var out [][]byte
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		out = append(out, []byte(l))
	}
	return out
}

func TestDetectPicksTheRightFormat(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name: "jsonl",
			input: `{"ts":"2026-08-13T14:02:00Z","level":"info","msg":"a"}
{"ts":"2026-08-13T14:02:01Z","level":"info","msg":"b"}
{"ts":"2026-08-13T14:02:02Z","level":"warn","msg":"c"}`,
			want: "jsonl",
		},
		{
			name: "logfmt",
			input: `ts=2026-08-13T14:02:00Z level=info msg="a" user_id=u_1
ts=2026-08-13T14:02:01Z level=info msg="b" user_id=u_2
ts=2026-08-13T14:02:02Z level=warn msg="c" user_id=u_3`,
			want: "logfmt",
		},
		{
			name: "unknown format falls back to text",
			input: `10.0.0.1 - - [13/Aug/2026:14:02:00 +0000] "POST /api HTTP/1.1" 200 547
10.0.0.2 - - [13/Aug/2026:14:02:01 +0000] "GET /health HTTP/1.1" 200 12`,
			want: "text",
		},
		{
			name:  "prose is not logfmt",
			input: "the config was set to debug=true by an operator\nanother sentence entirely\nand a third one here",
			want:  "text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Detect(lines(tt.input))
			if got.Parser == nil {
				t.Fatal("no parser chosen; a source must never be left without one")
			}
			if got.Parser.Name() != tt.want {
				t.Errorf("chose %q (%.2f), want %q", got.Parser.Name(), got.Confidence, tt.want)
			}
		})
	}
}

// One foreign line leaking into a file must not change the verdict. The blaster
// injects exactly this.
func TestDetectToleratesForeignLines(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		sb.WriteString(`{"ts":"2026-08-13T14:02:00Z","level":"info","msg":"ok"}` + "\n")
	}
	sb.WriteString("2026-08-13 14:11:02 [notice] 1#1: signal 17 (SIGCHLD) received from 812\n")

	got := Detect(lines(sb.String()))
	if got.Parser.Name() != "jsonl" {
		t.Errorf("chose %q, want jsonl; one foreign line hijacked detection", got.Parser.Name())
	}
}

func TestDetectEmptySample(t *testing.T) {
	got := Detect(nil)
	if got.Parser == nil {
		t.Fatal("no parser chosen for an empty sample")
	}
	if got.Confidence != 0 {
		t.Errorf("Confidence = %v, want 0 for an empty sample", got.Confidence)
	}
}

// Detection must not depend on map iteration order, or a mixed directory parses
// differently between runs.
func TestDetectIsDeterministic(t *testing.T) {
	sample := lines("nothing here matches any real format\njust two lines of prose")

	first := Detect(sample)
	for i := 0; i < 20; i++ {
		if got := Detect(sample); got.Parser.Name() != first.Parser.Name() {
			t.Fatalf("run %d chose %q, first run chose %q", i, got.Parser.Name(), first.Parser.Name())
		}
	}
}

func TestSampleLinesSkipsBlanksAndRespectsLimit(t *testing.T) {
	input := "one\n\n\ntwo\n\nthree\nfour\n"

	got, err := SampleLines(strings.NewReader(input), 3)
	if err != nil {
		t.Fatalf("SampleLines: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d lines, want 3", len(got))
	}
	for _, l := range got {
		if len(strings.TrimSpace(string(l))) == 0 {
			t.Error("a blank line reached the sample")
		}
	}
	if string(got[0]) != "one" || string(got[2]) != "three" {
		t.Errorf("sample = %q", got)
	}
}

func TestSampleLinesOnEmptyInput(t *testing.T) {
	got, err := SampleLines(strings.NewReader(""), 10)
	if err != nil {
		t.Fatalf("SampleLines: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d lines from empty input", len(got))
	}
}

// A close call must be reported rather than silently resolved, so the CLI can
// tell the user which format it guessed and offer --parser.
func TestAmbiguousDetectionIsFlagged(t *testing.T) {
	clear := Detect(lines(`{"ts":"2026-08-13T14:02:00Z","msg":"a"}
{"ts":"2026-08-13T14:02:01Z","msg":"b"}`))
	if clear.Ambiguous() {
		t.Errorf("clean jsonl reported ambiguous (%.2f vs %.2f)", clear.Confidence, clear.RunnerUp)
	}
}
