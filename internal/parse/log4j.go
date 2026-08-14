package parse

import (
	"bytes"
	"regexp"
	"strings"
	"time"
)

func init() { Register(&log4jParser{}) }

// log4jParser reads the common Log4j and Logback console patterns.
//
//	2026-08-13 14:12:48.146 [worker-1] ERROR c.a.p.ChargeHandler - read timed out
//	2026-08-13 14:12:48,146 ERROR [main] com.acme.Boot - started
//	14:12:48.146 [main] INFO  com.acme.Bar - message
//
// Java's default patterns write a local time with no offset at all, which is
// the trap in docs/FILTER-DSL.md section 2.5: if the server runs UTC and the
// reader's laptop does not, every record is displayed an hour out and nothing
// warns anybody. Timestamps from this parser are always reported as zoneless so
// the source's assumed zone applies and is disclosed.
//
// Stack traces make this the one v1 format whose records span lines.
type log4jParser struct{}

func (p *log4jParser) Name() string { return "log4j" }

// log4jRe matches the two common orderings of level and thread. Both are
// widespread and neither is worth a second parser.
var log4jRe = regexp.MustCompile(
	`^(\d{4}-\d{2}-\d{2}[ T])?(\d{2}:\d{2}:\d{2}[.,]\d{1,9})\s+` +
		`(?:\[([^\]]+)\]\s+)?` +
		`(TRACE|DEBUG|INFO|WARN|WARNING|ERROR|FATAL|SEVERE|FINE|FINEST)\s+` +
		`(?:\[([^\]]+)\]\s+)?` +
		`(?:([\w.$]+)\s*)?` +
		`(?:\[([^\]]*=[^\]]*)\]\s*)?` +
		`(?:-\s+)?` +
		`(.*)$`)

// mdcRe matches one key=value pair in a bracketed MDC section.
var mdcRe = regexp.MustCompile(`([\w.-]+)=([^,\]]*)`)

// continuationRe matches the shapes a Java stack trace uses.
//
// Matching on shape rather than only on indentation matters because "Caused
// by:" is written flush left by some layouts, and treating it as a new record
// would sever an exception from its cause.
var continuationRe = regexp.MustCompile(
	`^(?:\s+at\s|\s+\.{3}\s|\s*Caused by:|\s*Suppressed:|\s+\w+(?:\.\w+)*Exception)`)

func (p *log4jParser) Detect(sample [][]byte) float64 {
	if len(sample) == 0 {
		return 0
	}

	var considered, matched int
	for _, line := range sample {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		// Stack trace lines are part of a record, not failures to parse one.
		if p.IsContinuation(line) {
			continue
		}
		considered++
		if log4jRe.Match(line) {
			matched++
		}
	}
	if considered == 0 {
		return 0
	}
	return float64(matched) / float64(considered)
}

// IsContinuation reports whether a line belongs to the record above it.
func (p *log4jParser) IsContinuation(line []byte) bool {
	if len(line) == 0 {
		return false
	}
	// A line that starts a new record wins, even if it is indented, so an
	// oddly formatted log does not swallow everything after it.
	if log4jRe.Match(line) {
		return false
	}
	if line[0] == '\t' {
		return true
	}
	return continuationRe.Match(line)
}

func (p *log4jParser) Parse(line []byte) (Record, error) {
	m := log4jRe.FindSubmatch(line)
	if m == nil {
		return Record{}, ErrNoMatch
	}

	rec := Record{
		Level:   NormaliseLevel(string(m[4])),
		Message: string(m[8]),
		Fields:  make(map[string]any, 4),
	}

	// The MDC is where a Java application attaches a trace id, a tenant, or a
	// user. Promoting it to real fields is what lets trace_id:a91c40f2 reach a
	// Java service at all, rather than the id being buried in message text.
	parseMDC(string(m[7]), rec.Fields)

	// Log4j writes milliseconds after a comma in its older default pattern.
	stamp := string(m[1]) + string(bytes.Replace(m[2], []byte(","), []byte("."), 1))
	if m[1] == nil || len(m[1]) == 0 {
		// A time with no date. It cannot be placed without knowing the day, so
		// leave the record timestamp-less rather than inventing one: ts:none
		// finds it, and a made-up date would be worse than none.
		stamp = ""
	}

	if stamp != "" {
		// Always parsed as a wall clock in UTC and reported as unzoned. The
		// reader then applies the source's assumed zone.
		if ts, _, ok := ParseTime(normaliseLog4jStamp(stamp), time.UTC); ok {
			rec.Timestamp = ts
			rec.TimestampZoned = false
		}
	}

	// The thread appears in one bracket group or the other depending on the
	// pattern; only one can be present.
	if thread := firstNonEmpty(string(m[3]), string(m[5])); thread != "" {
		rec.Fields["thread"] = thread
	}
	if logger := string(m[6]); logger != "" {
		rec.Fields["logger"] = logger
	}

	return rec, nil
}

// parseMDC promotes a bracketed key=value section into fields.
//
// Written by %X in a Log4j pattern, e.g. [trace_id=a91c40f2, attempt=1].
func parseMDC(mdc string, fields map[string]any) {
	if mdc == "" {
		return
	}
	for _, m := range mdcRe.FindAllStringSubmatch(mdc, -1) {
		key, value := m[1], strings.TrimSpace(m[2])
		if value == "" {
			continue
		}
		fields[key] = typed(value)
	}
}

// normaliseLog4jStamp turns a T separator into a space so one layout covers
// both spellings.
func normaliseLog4jStamp(s string) string {
	if len(s) > 10 && s[10] == 'T' {
		return s[:10] + " " + s[11:]
	}
	return s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
