package parse

import (
	"strings"
	"testing"
	"time"
)

func TestNginxParse(t *testing.T) {
	p, _ := Get("nginx")

	t.Run("combined", func(t *testing.T) {
		rec := mustParse(t, p,
			`10.0.0.116 - alice [13/Aug/2026:14:02:00 +0000] "POST /api/cart HTTP/1.1" 200 547 "-" "checkout-web/2.1"`)

		want := time.Date(2026, 8, 13, 14, 2, 0, 0, time.UTC)
		if !rec.Timestamp.Equal(want) {
			t.Errorf("Timestamp = %v, want %v", rec.Timestamp, want)
		}
		// The bracketed time carries an explicit offset, so no assumption is
		// involved and nothing needs disclosing.
		if !rec.TimestampZoned {
			t.Error("an nginx timestamp carries +0000; it should be reported as zoned")
		}

		checkFields(t, rec, map[string]any{
			"client": "10.0.0.116",
			"user":   "alice",
			"method": "POST",
			"path":   "/api/cart",
			"status": int64(200),
			"bytes":  int64(547),
			"agent":  "checkout-web/2.1",
		})
		if rec.Message != "POST /api/cart" {
			t.Errorf("Message = %q", rec.Message)
		}
	})

	t.Run("common, without referer and agent", func(t *testing.T) {
		rec := mustParse(t, p,
			`10.0.0.1 - - [13/Aug/2026:14:02:00 +0000] "GET /healthz HTTP/1.1" 200 12`)
		if rec.Fields["path"] != "/healthz" {
			t.Errorf("path = %#v", rec.Fields["path"])
		}
		// A hyphen means absent. Storing it would be storing the absence of
		// data as data.
		if _, ok := rec.Fields["user"]; ok {
			t.Error("a hyphen user was stored as a value")
		}
		if _, ok := rec.Fields["agent"]; ok {
			t.Error("a missing agent was stored")
		}
	})

	// Access logs carry no severity, so it is derived from the status. Without
	// this, level:>=error misses the 502s that are half the incident.
	t.Run("level is derived from the status", func(t *testing.T) {
		tests := map[string]string{
			`10.0.0.1 - - [13/Aug/2026:14:02:00 +0000] "GET / HTTP/1.1" 200 1`: LevelInfo,
			`10.0.0.1 - - [13/Aug/2026:14:02:00 +0000] "GET / HTTP/1.1" 301 1`: LevelInfo,
			`10.0.0.1 - - [13/Aug/2026:14:02:00 +0000] "GET / HTTP/1.1" 404 1`: LevelWarn,
			`10.0.0.1 - - [13/Aug/2026:14:02:00 +0000] "GET / HTTP/1.1" 429 1`: LevelWarn,
			`10.0.0.1 - - [13/Aug/2026:14:02:00 +0000] "GET / HTTP/1.1" 500 1`: LevelError,
			`10.0.0.1 - - [13/Aug/2026:14:02:00 +0000] "GET / HTTP/1.1" 502 1`: LevelError,
		}
		for line, want := range tests {
			rec := mustParse(t, p, line)
			if rec.Level != want {
				t.Errorf("status %v gave level %q, want %q", rec.Fields["status"], rec.Level, want)
			}
		}
	})

	// Scanners send junk request lines. One must not fail the record.
	t.Run("malformed request line keeps the record", func(t *testing.T) {
		rec := mustParse(t, p,
			`10.0.0.1 - - [13/Aug/2026:14:02:00 +0000] "\x16\x03\x01" 400 0 "-" "-"`)
		if rec.Fields["status"] != int64(400) {
			t.Errorf("status = %#v", rec.Fields["status"])
		}
		if rec.Message == "" {
			t.Error("the raw request should still be the message")
		}
	})

	t.Run("rejects other formats", func(t *testing.T) {
		for _, line := range []string{
			`{"ts":"2026-08-13T14:00:00Z","msg":"a"}`,
			`2026-08-13 14:02:00.100 UTC [20353] LOG:  duration: 1ms`,
			``,
		} {
			if _, err := p.Parse([]byte(line)); err != ErrNoMatch {
				t.Errorf("Parse(%.40q) = %v, want ErrNoMatch", line, err)
			}
		}
	})
}

func TestSyslogParse(t *testing.T) {
	p, _ := Get("syslog")

	t.Run("full record", func(t *testing.T) {
		rec := mustParse(t, p,
			`<14>1 2026-08-13T14:02:00Z host-01 sshd 3344 - - session opened for user deploy`)

		if rec.Level != LevelInfo {
			t.Errorf("Level = %q, want info (severity 6)", rec.Level)
		}
		if rec.Message != "session opened for user deploy" {
			t.Errorf("Message = %q", rec.Message)
		}
		checkFields(t, rec, map[string]any{
			"host":     "host-01",
			"app":      "sshd",
			"pid":      int64(3344),
			"facility": int64(1),
			"severity": int64(6),
		})
		if !rec.TimestampZoned {
			t.Error("an RFC5424 timestamp carries a zone")
		}
	})

	// The priority encodes severity, which is the one machine-readable
	// severity in the whole format set.
	t.Run("priority maps to level", func(t *testing.T) {
		tests := map[int]string{
			8:  LevelFatal, // emergency
			9:  LevelFatal, // alert
			10: LevelFatal, // critical
			11: LevelError,
			12: LevelWarn,
			13: LevelInfo, // notice
			14: LevelInfo, // informational
			15: LevelDebug,
		}
		for pri, want := range tests {
			line := `<` + itoa(pri) + `>1 2026-08-13T14:02:00Z host app 1 - - msg`
			rec := mustParse(t, p, line)
			if rec.Level != want {
				t.Errorf("priority %d gave level %q, want %q", pri, rec.Level, want)
			}
		}
	})

	// A nil value in RFC5424 is a bare hyphen.
	t.Run("nil values are absent, not stored", func(t *testing.T) {
		rec := mustParse(t, p, `<14>1 2026-08-13T14:02:00Z host-01 kernel - - - kernel message`)
		if _, ok := rec.Fields["pid"]; ok {
			t.Error("a hyphen pid was stored as a value")
		}
		if rec.Message != "kernel message" {
			t.Errorf("Message = %q", rec.Message)
		}
	})

	// Structured data is the one place syslog lets an application attach named
	// values, so it must become real fields. Left as an opaque string,
	// trace_id:a91c40f2 could never reach a syslog source.
	t.Run("structured data becomes fields", func(t *testing.T) {
		rec := mustParse(t, p,
			`<14>1 2026-08-13T14:02:00Z host app 1 ID47 [trace@32473 trace_id="a91c40f2" iut="3"] the message`)

		checkFields(t, rec, map[string]any{
			"trace_id": "a91c40f2",
			"iut":      int64(3),
			"sd_id":    "trace@32473",
		})
		if rec.Message != "the message" {
			t.Errorf("Message = %q", rec.Message)
		}
	})

	t.Run("escaped characters inside structured data", func(t *testing.T) {
		rec := mustParse(t, p,
			`<14>1 2026-08-13T14:02:00Z host app 1 - [ex@1 msg="he said \"stop\"" path="a\]b"] done`)
		if rec.Fields["msg"] != `he said "stop"` {
			t.Errorf("msg = %#v", rec.Fields["msg"])
		}
		if rec.Fields["path"] != "a]b" {
			t.Errorf("path = %#v", rec.Fields["path"])
		}
	})

	// Unparseable structured data is still data.
	t.Run("malformed structured data is kept whole", func(t *testing.T) {
		rec := mustParse(t, p,
			`<14>1 2026-08-13T14:02:00Z host app 1 - [this is not valid sd] the message`)
		if rec.Fields["structured_data"] == nil && rec.Fields["sd_id"] == nil {
			t.Error("malformed structured data was dropped entirely")
		}
	})

	t.Run("rejects other formats", func(t *testing.T) {
		for _, line := range []string{`not syslog`, `<999 broken`, ``} {
			if _, err := p.Parse([]byte(line)); err != ErrNoMatch {
				t.Errorf("Parse(%q) = %v, want ErrNoMatch", line, err)
			}
		}
	})
}

func TestPostgresParse(t *testing.T) {
	p, _ := Get("postgres")

	t.Run("full record", func(t *testing.T) {
		rec := mustParse(t, p,
			`2026-08-13 14:02:00.100 UTC [20353] LOG:  duration: 178.328 ms  statement: SELECT 1`)

		if rec.Level != LevelInfo {
			t.Errorf("Level = %q, want info (LOG normalises to info)", rec.Level)
		}
		// LOG, STATEMENT, and DETAIL all normalise to info but mean different
		// things, so the original word is kept.
		if rec.Fields["pg_severity"] != "LOG" {
			t.Errorf("pg_severity = %#v, want LOG", rec.Fields["pg_severity"])
		}
		if rec.Fields["pid"] != int64(20353) {
			t.Errorf("pid = %#v", rec.Fields["pid"])
		}
		if !strings.HasPrefix(rec.Message, "duration:") {
			t.Errorf("Message = %q", rec.Message)
		}
	})

	t.Run("severities", func(t *testing.T) {
		tests := map[string]string{
			"LOG":     LevelInfo,
			"NOTICE":  LevelInfo,
			"WARNING": LevelWarn,
			"ERROR":   LevelError,
			"FATAL":   LevelFatal,
			"PANIC":   LevelFatal,
			"DEBUG1":  LevelDebug,
		}
		for severity, want := range tests {
			line := `2026-08-13 14:02:00.100 UTC [1] ` + severity + `:  message`
			rec := mustParse(t, p, line)
			if rec.Level != want {
				t.Errorf("%s gave level %q, want %q", severity, rec.Level, want)
			}
		}
	})

	t.Run("user and database", func(t *testing.T) {
		rec := mustParse(t, p,
			`2026-08-13 14:02:00.100 UTC [20353] alice@checkout ERROR:  relation "x" does not exist`)
		checkFields(t, rec, map[string]any{"user": "alice", "db": "checkout"})
	})

	// UTC is unambiguous. Any other abbreviation is not — CST and IST each name
	// more than one zone — so those must be reported as unzoned and left to the
	// source's assumed zone.
	t.Run("zone abbreviation handling", func(t *testing.T) {
		tests := []struct {
			line      string
			wantZoned bool
		}{
			{`2026-08-13 14:02:00.100 UTC [1] LOG:  a`, true},
			{`2026-08-13 14:02:00.100 GMT [1] LOG:  a`, true},
			{`2026-08-13 14:02:00.100 CST [1] LOG:  a`, false},
			{`2026-08-13 14:02:00.100 IST [1] LOG:  a`, false},
			{`2026-08-13 14:02:00.100 [1] LOG:  a`, false},
			{`2026-08-13 14:02:00.100 +05:30 [1] LOG:  a`, true},
		}
		for _, tt := range tests {
			rec := mustParse(t, p, tt.line)
			if rec.TimestampZoned != tt.wantZoned {
				t.Errorf("%.45q: TimestampZoned = %v, want %v",
					tt.line, rec.TimestampZoned, tt.wantZoned)
			}
		}
	})

	t.Run("continuation lines are recognised", func(t *testing.T) {
		c, ok := p.(Continuer)
		if !ok {
			t.Fatal("the postgres parser should implement Continuer")
		}
		if !c.IsContinuation([]byte("\tFROM orders WHERE id = 1")) {
			t.Error("a tab-indented statement continuation was not recognised")
		}
		if c.IsContinuation([]byte(`2026-08-13 14:02:00.100 UTC [1] LOG:  a`)) {
			t.Error("a real record was treated as a continuation")
		}
	})

	t.Run("rejects other formats", func(t *testing.T) {
		for _, line := range []string{
			`2026-08-13 14:12:48.146 [worker-1] ERROR c.a.p.Handler - not postgres`,
			`plain text`,
		} {
			if _, err := p.Parse([]byte(line)); err != ErrNoMatch {
				t.Errorf("Parse(%.40q) = %v, want ErrNoMatch", line, err)
			}
		}
	})
}

func TestLog4jParse(t *testing.T) {
	p, _ := Get("log4j")

	t.Run("full record", func(t *testing.T) {
		rec := mustParse(t, p,
			`2026-08-13 14:12:48.146 [worker-1] ERROR c.a.p.ChargeHandler - PaymentGatewayException: read timed out`)

		if rec.Level != LevelError {
			t.Errorf("Level = %q", rec.Level)
		}
		if rec.Message != "PaymentGatewayException: read timed out" {
			t.Errorf("Message = %q", rec.Message)
		}
		checkFields(t, rec, map[string]any{
			"thread": "worker-1",
			"logger": "c.a.p.ChargeHandler",
		})

		want := time.Date(2026, 8, 13, 14, 12, 48, 146000000, time.UTC)
		if !rec.Timestamp.Equal(want) {
			t.Errorf("Timestamp = %v, want %v", rec.Timestamp, want)
		}
	})

	// Java's default patterns write no offset at all. Reporting these as zoned
	// would hide the assumption that section 2.5 exists to surface.
	t.Run("timestamps are always reported as unzoned", func(t *testing.T) {
		rec := mustParse(t, p,
			`2026-08-13 14:12:48.146 [worker-1] ERROR c.a.p.Handler - boom`)
		if rec.TimestampZoned {
			t.Error("log4j carries no offset; the timestamp must be reported as assumed")
		}
	})

	t.Run("comma milliseconds and level before thread", func(t *testing.T) {
		rec := mustParse(t, p,
			`2026-08-13 14:12:48,146 ERROR [main] com.acme.Boot - started`)
		if rec.Level != LevelError {
			t.Errorf("Level = %q", rec.Level)
		}
		if rec.Fields["thread"] != "main" {
			t.Errorf("thread = %#v", rec.Fields["thread"])
		}
		if rec.Timestamp.IsZero() {
			t.Error("comma-separated milliseconds were not parsed")
		}
	})

	// A time with no date cannot be placed. Inventing one would be worse than
	// leaving the record findable through ts:none.
	t.Run("time without a date leaves no timestamp", func(t *testing.T) {
		rec := mustParse(t, p, `14:12:48.146 [main] INFO  com.acme.Bar - message`)
		if rec.HasTimestamp() {
			t.Errorf("a dateless time produced %v; it should have none", rec.Timestamp)
		}
		if rec.Level != LevelInfo {
			t.Errorf("Level = %q; the rest of the record should still parse", rec.Level)
		}
	})

	t.Run("stack trace continuation", func(t *testing.T) {
		c, ok := p.(Continuer)
		if !ok {
			t.Fatal("the log4j parser should implement Continuer")
		}

		continuations := []string{
			"\tat com.acme.pay.GatewayClient.charge(GatewayClient.java:214)",
			"\tCaused by: java.net.SocketTimeoutException: Read timed out",
			"\t\t... 14 more",
			"Caused by: java.lang.IllegalStateException",
			"  at com.acme.Foo.bar(Foo.java:1)",
		}
		for _, line := range continuations {
			if !c.IsContinuation([]byte(line)) {
				t.Errorf("not recognised as a continuation: %q", line)
			}
		}

		// A real record must never be swallowed, even indented.
		if c.IsContinuation([]byte(`2026-08-13 14:12:49.000 [w] INFO  c.a.X - next`)) {
			t.Error("a real record was treated as a continuation")
		}
	})

	t.Run("rejects other formats", func(t *testing.T) {
		for _, line := range []string{
			`2026-08-13 14:02:00.100 UTC [20353] LOG:  not log4j`,
			`{"ts":"2026-08-13T14:00:00Z"}`,
			`plain text`,
		} {
			if _, err := p.Parse([]byte(line)); err != ErrNoMatch {
				t.Errorf("Parse(%.40q) = %v, want ErrNoMatch", line, err)
			}
		}
	})
}

// The formats must not claim each other's lines. A postgres line reaching the
// log4j parser, or the reverse, would silently mangle a mixed directory.
func TestParsersDoNotClaimEachOther(t *testing.T) {
	samples := map[string]string{
		"jsonl":    `{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"a"}`,
		"logfmt":   `ts=2026-08-13T14:00:00Z level=info msg="a" user_id=u_1`,
		"nginx":    `10.0.0.1 - - [13/Aug/2026:14:02:00 +0000] "GET / HTTP/1.1" 200 12 "-" "curl/8"`,
		"syslog":   `<14>1 2026-08-13T14:02:00Z host-01 sshd 3344 - - session opened`,
		"postgres": `2026-08-13 14:02:00.100 UTC [20353] LOG:  duration: 178.328 ms`,
		"log4j":    `2026-08-13 14:12:48.146 [worker-1] ERROR c.a.p.Handler - read timed out`,
		"cri":      `2026-08-13T14:02:00.113456789Z stdout F listening on :8080`,
		"docker":   `{"log":"listening on :8080\n","stream":"stdout","time":"2026-08-13T14:02:00.113456789Z"}`,
		"journald": `{"__REALTIME_TIMESTAMP":"1786629720000000","PRIORITY":"6","MESSAGE":"Started Checkout API.","_HOSTNAME":"node-01"}`,
	}

	for owner, line := range samples {
		t.Run(owner, func(t *testing.T) {
			// Detection over a sample of this one format must pick its owner.
			sample := [][]byte{[]byte(line), []byte(line), []byte(line)}
			got := Detect(sample)
			if got.Parser.Name() != owner {
				t.Errorf("a %s line was detected as %s (%.2f)", owner, got.Parser.Name(), got.Confidence)
			}
		})
	}
}

func checkFields(t *testing.T, rec Record, want map[string]any) {
	t.Helper()
	for key, value := range want {
		got, ok := rec.Fields[key]
		if !ok {
			t.Errorf("field %q missing (have %v)", key, fieldNames(rec.Fields))
			continue
		}
		if got != value {
			t.Errorf("field %q = %#v, want %#v", key, got, value)
		}
	}
}

func fieldNames(fields map[string]any) []string {
	out := make([]string, 0, len(fields))
	for k := range fields {
		out = append(out, k)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// The MDC is where a Java application attaches a trace id. Without promoting
// it, trace_id:a91c40f2 cannot reach a Java service.
func TestLog4jMDC(t *testing.T) {
	p, _ := Get("log4j")

	t.Run("mdc becomes fields", func(t *testing.T) {
		rec := mustParse(t, p,
			`2026-08-13 14:12:48.146 [worker-1] ERROR c.a.p.ChargeHandler [trace_id=a91c40f2, attempt=2] - read timed out`)

		checkFields(t, rec, map[string]any{
			"trace_id": "a91c40f2",
			"attempt":  int64(2),
			"thread":   "worker-1",
			"logger":   "c.a.p.ChargeHandler",
		})
		if rec.Message != "read timed out" {
			t.Errorf("Message = %q", rec.Message)
		}
		if rec.Level != LevelError {
			t.Errorf("Level = %q", rec.Level)
		}
	})

	// A pattern with no MDC section must be unaffected.
	t.Run("no mdc section", func(t *testing.T) {
		rec := mustParse(t, p,
			`2026-08-13 14:12:48.146 [worker-1] ERROR c.a.p.ChargeHandler - read timed out`)
		if rec.Message != "read timed out" {
			t.Errorf("Message = %q", rec.Message)
		}
		if rec.Fields["logger"] != "c.a.p.ChargeHandler" {
			t.Errorf("logger = %#v", rec.Fields["logger"])
		}
	})

	// A bracketed section that is not key=value is not an MDC, and must stay in
	// the message rather than being silently swallowed.
	t.Run("a non-mdc bracket stays in the message", func(t *testing.T) {
		rec := mustParse(t, p,
			`2026-08-13 14:12:48.146 [worker-1] INFO  c.a.p.Worker - [batch 7] consumed`)
		if !strings.Contains(rec.Message, "batch 7") {
			t.Errorf("Message = %q, want the bracket kept", rec.Message)
		}
	})
}
