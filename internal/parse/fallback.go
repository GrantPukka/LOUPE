package parse

import (
	"bytes"
	"regexp"
	"time"
)

func init() { Register(&fallbackParser{}) }

// fallbackParser handles anything no other parser claims. It keeps the whole
// line as the message and makes a best effort at a leading timestamp and an
// embedded level.
//
// It exists so that pointing loupe at an unknown format still produces a usable
// timeline rather than an error. It never returns ErrNoMatch.
type fallbackParser struct{}

func (p *fallbackParser) Name() string { return "text" }

// Detect always returns a small non-zero confidence. It must be low enough to
// lose to any real parser and high enough to win when nothing else matches.
func (p *fallbackParser) Detect(sample [][]byte) float64 {
	if len(sample) == 0 {
		return 0
	}
	return 0.01
}

// leadingTimestampRe matches the common timestamp shapes when they appear at
// the start of a line. Anchored on purpose: a date in the middle of a message
// is part of the message, not the record's time.
var leadingTimestampRe = regexp.MustCompile(
	`^\[?(` +
		`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?` +
		`|\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}` +
		`|\d{2}/\w{3}/\d{4}:\d{2}:\d{2}:\d{2} [+-]\d{4}` +
		`|\w{3} [ \d]\d \d{2}:\d{2}:\d{2}` +
		`)\]?`)

// embeddedLevelRe finds a severity word near the start of a line, in the
// bracketed or bare forms most text logs use.
var embeddedLevelRe = regexp.MustCompile(
	`(?i)[\[\s|]?\b(TRACE|DEBUG|INFO|INFORMATION|NOTICE|WARN|WARNING|ERROR|SEVERE|FATAL|CRITICAL|PANIC|EMERG(?:ENCY)?|ALERT)\b[\]\s:|]?`)

func (p *fallbackParser) Parse(line []byte) (Record, error) {
	trimmed := bytes.TrimRight(line, "\r\n")
	rec := Record{
		Message: string(trimmed),
		Fields:  map[string]any{},
	}

	if m := leadingTimestampRe.FindSubmatch(trimmed); m != nil {
		if ts, zoned, ok := ParseTime(string(m[1]), time.UTC); ok {
			rec.Timestamp, rec.TimestampZoned = ts, zoned
			// The timestamp is metadata, not part of what the operator wants to
			// read, so strip it from the message.
			rec.Message = string(bytes.TrimSpace(trimmed[len(m[0]):]))
		}
	}

	rec.Level = levelFromMessage(rec.Message)

	return rec, nil
}

// levelFromMessage reads a severity out of the text of a message.
//
// Shared with the container formats, which carry no severity field of their
// own: Docker and CRI record only which stream a line came from, and inferring
// the level from stderr would mark ordinary progress output as an error and
// poison level:>=error for the whole source. Reading the word the program
// actually wrote is a better-founded guess, and it is the same guess the
// fallback parser has always made.
//
// Only the front of the message is searched. A line mentioning the word "error"
// three sentences in is not an error-level record.
func levelFromMessage(msg string) string {
	const head = 64
	if len(msg) > head {
		msg = msg[:head]
	}

	if m := embeddedLevelRe.FindStringSubmatch(msg); m != nil {
		return NormaliseLevel(m[1])
	}
	return ""
}
