package parse

import (
	"strings"
	"testing"
	"time"
)

func TestNormaliseLevel(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		// The cross-format filtering case: these must all compare equal.
		{"WARN", LevelWarn},
		{"warn", LevelWarn},
		{"Warning", LevelWarn},
		{"W", LevelWarn},
		{"  WARNING  ", LevelWarn},
		{"[WARN]", LevelWarn},

		{"ERROR", LevelError},
		{"err", LevelError},
		{"E", LevelError},
		{"SEVERE", LevelError},

		{"INFO", LevelInfo},
		{"notice", LevelInfo},
		{"informational", LevelInfo},

		{"DEBUG", LevelDebug},
		{"FINE", LevelDebug},
		{"TRACE", LevelTrace},
		{"verbose", LevelTrace},

		{"FATAL", LevelFatal},
		{"crit", LevelFatal},
		{"emerg", LevelFatal},
		{"panic", LevelFatal},

		// An unknown level keeps its original text rather than being guessed at.
		{"AUDIT", "audit"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := NormaliseLevel(tt.in); got != tt.want {
				t.Errorf("NormaliseLevel(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// level:>=warn depends entirely on this ordering.
func TestLevelOrdering(t *testing.T) {
	want := []string{"trace", "debug", "info", "warn", "error", "fatal"}
	for i := 1; i < len(want); i++ {
		lo, okLo := LevelRank(want[i-1])
		hi, okHi := LevelRank(want[i])
		if !okLo || !okHi {
			t.Fatalf("%s or %s is not ranked", want[i-1], want[i])
		}
		if lo >= hi {
			t.Errorf("%s should rank below %s", want[i-1], want[i])
		}
	}

	// An unranked level must not be swept up by a comparison filter.
	if _, ok := LevelRank("audit"); ok {
		t.Error("an unrecognised level should not be ranked")
	}
}

func TestRegistryHasTheV1Parsers(t *testing.T) {
	for _, name := range []string{"jsonl", "logfmt", "text"} {
		if _, ok := Get(name); !ok {
			t.Errorf("parser %q not registered", name)
		}
	}
	if _, ok := Get("nope"); ok {
		t.Error("Get returned a parser that does not exist")
	}
}

// All must be deterministically ordered, or detection ties break differently
// between runs and a mixed directory parses differently each time.
func TestAllIsSorted(t *testing.T) {
	got := Names()
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("Names() not sorted: %v", got)
		}
	}
}

func mustParse(t *testing.T, p Parser, line string) Record {
	t.Helper()
	rec, err := p.Parse([]byte(line))
	if err != nil {
		t.Fatalf("Parse(%q): %v", line, err)
	}
	return rec
}

func TestJSONLParse(t *testing.T) {
	p, _ := Get("jsonl")

	t.Run("full record", func(t *testing.T) {
		rec := mustParse(t, p, `{"ts":"2026-08-13T14:02:00.021Z","level":"error","msg":"boom","status":502,"trace_id":"a91c40f2"}`)

		if !rec.Timestamp.Equal(time.Date(2026, 8, 13, 14, 2, 0, 21000000, time.UTC)) {
			t.Errorf("Timestamp = %v", rec.Timestamp)
		}
		if !rec.TimestampZoned {
			t.Error("an RFC3339 timestamp with a Z should be reported as zoned")
		}
		if rec.Level != LevelError {
			t.Errorf("Level = %q", rec.Level)
		}
		if rec.Message != "boom" {
			t.Errorf("Message = %q", rec.Message)
		}
		if rec.Fields["status"] != int64(502) {
			t.Errorf("status = %#v, want int64(502)", rec.Fields["status"])
		}
		if rec.Fields["trace_id"] != "a91c40f2" {
			t.Errorf("trace_id = %#v", rec.Fields["trace_id"])
		}
	})

	// A timestamp with no offset means the displayed time depends on an
	// assumption, and the user has to be told.
	t.Run("timestamp without a zone is flagged", func(t *testing.T) {
		rec := mustParse(t, p, `{"ts":"2026-08-13T14:02:00","msg":"no offset"}`)
		if rec.Timestamp.IsZero() {
			t.Fatal("timestamp not parsed")
		}
		if rec.TimestampZoned {
			t.Error("a timestamp with no offset must not be reported as zoned")
		}
	})

	t.Run("alternative key names", func(t *testing.T) {
		rec := mustParse(t, p, `{"@timestamp":"2026-08-13T14:02:00Z","severity":"WARNING","message":"hi"}`)
		if rec.Timestamp.IsZero() {
			t.Error("@timestamp not recognised")
		}
		if rec.Level != LevelWarn {
			t.Errorf("Level = %q, want warn", rec.Level)
		}
		if rec.Message != "hi" {
			t.Errorf("Message = %q", rec.Message)
		}
	})

	t.Run("capitalised keys", func(t *testing.T) {
		rec := mustParse(t, p, `{"Level":"error","Message":"case insensitive"}`)
		if rec.Level != LevelError {
			t.Errorf("Level = %q; key matching should be case-insensitive", rec.Level)
		}
	})

	// Nothing may be dropped, including nested objects.
	t.Run("nested objects are kept", func(t *testing.T) {
		rec := mustParse(t, p, `{"msg":"x","http":{"status":500,"method":"GET"}}`)
		nested, ok := rec.Fields["http"].(map[string]any)
		if !ok {
			t.Fatalf("http = %#v, want a nested map", rec.Fields["http"])
		}
		if nested["status"] != int64(500) {
			t.Errorf("http.status = %#v", nested["status"])
		}
	})

	// Large integer IDs must survive. float64 would re-render this in
	// scientific notation and silently corrupt an equality filter.
	t.Run("large integers keep precision", func(t *testing.T) {
		rec := mustParse(t, p, `{"msg":"x","span_id":7241398234123456789}`)
		if rec.Fields["span_id"] != int64(7241398234123456789) {
			t.Errorf("span_id = %#v, want the exact integer", rec.Fields["span_id"])
		}
	})

	// A ts field that is not a timestamp must stay a field, not vanish.
	t.Run("unparseable ts value is kept as a field", func(t *testing.T) {
		rec := mustParse(t, p, `{"ts":"not a time","msg":"x"}`)
		if !rec.Timestamp.IsZero() {
			t.Error("garbage parsed as a timestamp")
		}
		if rec.Fields["ts"] != "not a time" {
			t.Errorf("ts field dropped: %#v", rec.Fields["ts"])
		}
	})

	t.Run("rejects non-json", func(t *testing.T) {
		for _, line := range []string{"", "not json", `{"unterminated": `} {
			if _, err := p.Parse([]byte(line)); err != ErrNoMatch {
				t.Errorf("Parse(%q) error = %v, want ErrNoMatch", line, err)
			}
		}
	})

	// Valid JSON that is not a log line still has to produce something.
	t.Run("json with no log shape becomes the message", func(t *testing.T) {
		rec := mustParse(t, p, `{"a":1,"b":2}`)
		if rec.Message == "" {
			t.Error("message is empty; the line would be invisible")
		}
	})
}

func TestLogfmtParse(t *testing.T) {
	p, _ := Get("logfmt")

	t.Run("full record", func(t *testing.T) {
		rec := mustParse(t, p, `ts=2026-08-13T14:02:00Z level=info msg="token validated" user_id=u_9329 ttl_s=3600`)

		if rec.Timestamp.IsZero() {
			t.Error("timestamp not parsed")
		}
		if rec.Level != LevelInfo {
			t.Errorf("Level = %q", rec.Level)
		}
		if rec.Message != "token validated" {
			t.Errorf("Message = %q", rec.Message)
		}
		if rec.Fields["user_id"] != "u_9329" {
			t.Errorf("user_id = %#v", rec.Fields["user_id"])
		}
		if rec.Fields["ttl_s"] != int64(3600) {
			t.Errorf("ttl_s = %#v, want int64 so numeric comparison works", rec.Fields["ttl_s"])
		}
	})

	t.Run("quoted values with escapes", func(t *testing.T) {
		rec := mustParse(t, p, `level=error msg="he said \"stop\"" path="/a b/c"`)
		if rec.Message != `he said "stop"` {
			t.Errorf("Message = %q", rec.Message)
		}
		if rec.Fields["path"] != "/a b/c" {
			t.Errorf("path = %#v", rec.Fields["path"])
		}
	})

	// Truncated lines are the blaster's first kind of damage.
	t.Run("unterminated quote does not fail the record", func(t *testing.T) {
		rec := mustParse(t, p, `level=error msg="read timed ou`)
		if rec.Level != LevelError {
			t.Errorf("Level = %q; the parseable prefix should survive", rec.Level)
		}
		if rec.Message != "read timed ou" {
			t.Errorf("Message = %q", rec.Message)
		}
	})

	t.Run("bare words become the message", func(t *testing.T) {
		rec := mustParse(t, p, `level=warn something happened key=value`)
		if !strings.Contains(rec.Message, "something") {
			t.Errorf("Message = %q, want the bare words kept", rec.Message)
		}
		if rec.Fields["key"] != "value" {
			t.Errorf("key = %#v", rec.Fields["key"])
		}
	})

	// An ID with a leading zero is not a number. Converting it would break an
	// equality filter on the original text.
	t.Run("leading zeros stay strings", func(t *testing.T) {
		rec := mustParse(t, p, `msg=x code=007 real=7`)
		if rec.Fields["code"] != "007" {
			t.Errorf("code = %#v, want the string \"007\"", rec.Fields["code"])
		}
		if rec.Fields["real"] != int64(7) {
			t.Errorf("real = %#v, want int64(7)", rec.Fields["real"])
		}
	})

	t.Run("booleans and floats", func(t *testing.T) {
		rec := mustParse(t, p, `msg=x enabled=true ratio=0.75`)
		if rec.Fields["enabled"] != true {
			t.Errorf("enabled = %#v", rec.Fields["enabled"])
		}
		if rec.Fields["ratio"] != 0.75 {
			t.Errorf("ratio = %#v", rec.Fields["ratio"])
		}
	})

	t.Run("rejects empty", func(t *testing.T) {
		if _, err := p.Parse([]byte("   ")); err != ErrNoMatch {
			t.Errorf("error = %v, want ErrNoMatch", err)
		}
	})
}

func TestFallbackParserNeverFails(t *testing.T) {
	p, _ := Get("text")

	lines := []string{
		"just some text",
		"2026-08-13 14:11:02 [notice] 1#1: signal 17 (SIGCHLD) received from 812",
		"\x00\x00binary junk\x00",
		"",
		strings.Repeat("x", 5000),
	}

	for _, line := range lines {
		if _, err := p.Parse([]byte(line)); err != nil {
			t.Errorf("Parse(%.30q) returned %v; the fallback must never fail", line, err)
		}
	}
}

func TestFallbackExtractsLeadingTimestampAndLevel(t *testing.T) {
	p, _ := Get("text")

	tests := []struct {
		name    string
		line    string
		wantTS  bool
		wantLvl string
		wantMsg string
	}{
		{
			name:    "iso timestamp and bracketed level",
			line:    "2026-08-13 14:11:02 [ERROR] something failed",
			wantTS:  true,
			wantLvl: LevelError,
			wantMsg: "[ERROR] something failed",
		},
		{
			name:    "nginx error log style",
			line:    "2026/08/13 14:11:02 [notice] 1#1: signal 17 received",
			wantTS:  true,
			wantLvl: LevelInfo,
		},
		{
			name:    "no timestamp keeps the whole line",
			line:    "WARN something is off",
			wantLvl: LevelWarn,
			wantMsg: "WARN something is off",
		},
		{
			// A date inside a sentence is part of the sentence.
			name:    "mid-line date is not the timestamp",
			line:    "backup completed for 2026-08-13 successfully",
			wantTS:  false,
			wantMsg: "backup completed for 2026-08-13 successfully",
		},
		{
			// The word error appearing late in a message is not a level.
			name: "level word far into the message is ignored",
			line: "the operation completed and there was nothing anyone could call an error here at all",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := mustParse(t, p, tt.line)
			if got := rec.HasTimestamp(); got != tt.wantTS {
				t.Errorf("HasTimestamp() = %v, want %v", got, tt.wantTS)
			}
			if rec.Level != tt.wantLvl {
				t.Errorf("Level = %q, want %q", rec.Level, tt.wantLvl)
			}
			if tt.wantMsg != "" && rec.Message != tt.wantMsg {
				t.Errorf("Message = %q, want %q", rec.Message, tt.wantMsg)
			}
		})
	}
}
