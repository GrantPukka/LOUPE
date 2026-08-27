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
//
// The timestamp may be bracketed: that is what Kafka, ZooKeeper and most of the
// Apache Java estate write, and it is the same Log4j pattern with %d wrapped in
// brackets rather than a format of its own.
//
// What follows the level is left to splitLogger rather than being teased apart
// here. A regex that tries to recognise an optional logger, an optional MDC and
// an optional separator in one pass will happily read the first word of an
// ordinary message as the logger name, which is what it used to do.
var log4jRe = regexp.MustCompile(
	`^\[?(\d{4}-\d{2}-\d{2}[ T])?(\d{2}:\d{2}:\d{2}[.,]\d{1,9})\]?\s+` +
		`(?:([\w.-]+)\s+)?` +
		`(?:\[([^\]=]+)\]\s+)?` +
		`(TRACE|DEBUG|INFO|WARN|WARNING|ERROR|FATAL|SEVERE|FINE|FINEST)\s+` +
		`(?:\[([^\]=]+)\]\s+)?` +
		`(.*)$`)

// loggerRe matches a logger name that is followed by proof it is one.
//
// Either a `[pid]` or `[thread]` suffix, or Log4j's own ` - ` separator before
// the message. Without that proof the token is just the message's first word:
// `notify-worker` in `ERROR notify-worker[3257] drain failed` is a logger, and
// `Shrinking` in `INFO Shrinking ISR from 1,2,3 to 1,2` is not.
//
// Hyphens are inside the character class deliberately. Excluding them was the
// old behaviour, and it turned every `notify-worker` into a logger named
// `notify` and a message beginning with a stray hyphen — while `loupe fields`
// reported one distinct logger across the whole source, which reads exactly
// like a correctly parsed single-logger application.
var loggerRe = regexp.MustCompile(`^([\w.$-]+)(?:\[([^\]]+)\])?\s+`)

// mdcSectionRe matches a bracketed MDC section, which is distinguished from a
// thread name by containing an equals sign.
var mdcSectionRe = regexp.MustCompile(`^\[([^\]]*=[^\]]*)\]\s*`)

// mdcRe matches one key=value pair in a bracketed MDC section.
var mdcRe = regexp.MustCompile(`([\w.-]+)=([^,\]]*)`)

// continuationRe matches the shapes a stack trace uses, in both languages that
// print one into a log stream.
//
// Matching on shape rather than only on indentation matters because "Caused
// by:" and Python's final `SomeError: message` are both written flush left, and
// treating either as a new record would sever an exception from its cause.
//
// Python is here rather than in a parser of its own because a traceback is not
// a log format: it is what a Python service prints *after* a log line, on the
// same handler, and the line above it is already parsed by whatever wrote it.
// The 8,640 traceback lines in a merged platform log belong to the ERROR record
// above them exactly as a Java stack trace does.
var continuationRe = regexp.MustCompile(
	`^(?:` +
		`\s+at\s|\s+\.{3}\s|\s*Caused by:|\s*Suppressed:|\s+\w+(?:\.\w+)*Exception` +
		`|Traceback \(most recent call last\):` + // python, opens the block
		`|\s+File "|\s{4,}\S` + // python frames and the source lines under them
		// The exception line that closes a Python traceback, and the one Java
		// prints flush left under the log line that reported it. Both are only
		// a continuation because they name an exception type: a `$` for a Java
		// inner class and a dotted package are part of that name.
		`|\w+(?:[.$]\w+)*(?:Error|Exception)(?:[.$]\w+)*:` +
		`|\w+(?:\.\w+)*(?:Exit|Interrupt|Warning):` +
		`)`)

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
		Level:  NormaliseLevel(string(m[5])),
		Fields: make(map[string]any, 6),
	}

	rec.Message = splitLogger(string(m[7]), rec.Fields)

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
	// Spring Boot's default pattern puts the host between the timestamp and the
	// thread. It is a real field and the reason 3,013 Spring lines used to match
	// nothing at all.
	setIfPresent(rec.Fields, "host", string(m[3]))

	if thread := firstNonEmpty(string(m[4]), string(m[6])); thread != "" {
		putField(rec.Fields, "thread", thread)
	}

	return rec, nil
}

// splitLogger takes what follows the level and separates the logger name, the
// MDC and the message, returning the message.
//
// A token is only read as a logger when the line proves it is one: a bracketed
// thread or pid after it, an MDC section after it, or Log4j's own " - "
// separator before the message. Without that proof it is the message's first
// word. That distinction is the whole fix — `notify-worker` in
// "ERROR notify-worker[3257] drain failed" is a logger, and `Shrinking` in
// "INFO Shrinking ISR from 1,2,3 to 1,2" is not, and a regex that simply took
// the first word reported the second one as a logger on every Kafka line.
func splitLogger(rest string, fields map[string]any) string {
	// A few patterns put the MDC before the logger rather than after it.
	rest = takeMDC(rest, fields)

	m := loggerRe.FindStringSubmatch(rest)
	if m == nil {
		return rest
	}

	after := rest[len(m[0]):]
	afterMDC := takeMDC(after, fields)
	separated := strings.HasPrefix(afterMDC, "- ")

	if m[2] == "" && afterMDC == after && !separated {
		return rest
	}

	putField(fields, "logger", m[1])
	if bracketed := m[2]; bracketed != "" {
		// Java writes the thread here; a Python or Go logger writes the pid.
		// setIfPresent types a run of digits as a number, which is what makes
		// pid:>20000 comparable.
		setIfPresent(fields, loggerBracketField(bracketed), bracketed)
	}

	if separated {
		return strings.TrimSpace(afterMDC[1:])
	}
	return afterMDC
}

// takeMDC consumes a leading MDC section, if there is one, into fields.
func takeMDC(s string, fields map[string]any) string {
	m := mdcSectionRe.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	parseMDC(m[1], fields)
	return s[len(m[0]):]
}

// loggerBracketField names the bracketed suffix after a logger by what it
// looks like. All digits is a process id; anything else is a thread name.
func loggerBracketField(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return "thread"
		}
	}
	return "pid"
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
		putField(fields, key, typed(value))
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
