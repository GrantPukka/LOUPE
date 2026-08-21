package parse

import (
	"bytes"
	"encoding/json"
	"strconv"
	"time"
)

func init() { Register(&journaldParser{}) }

// journaldParser reads `journalctl -o json`, one JSON object per line.
//
// It is a separate parser from jsonl rather than a set of extra key names in
// it, because almost nothing about a journal entry follows the conventions
// jsonl assumes: the timestamp is microseconds since the epoch in a string, the
// level is a syslog priority digit, and the message can arrive as an array of
// bytes. Teaching the generic parser those three exceptions would make every
// other JSON log pay for them.
type journaldParser struct{}

func (p *journaldParser) Name() string { return "journald" }

// journaldMarkers are the keys systemd adds to every entry it exports.
//
// The double underscore is the point: it is reserved for journal metadata, so
// no ordinary application log has it and a match is close to conclusive.
var journaldMarkers = [][]byte{
	[]byte(`"__REALTIME_TIMESTAMP"`),
	[]byte(`"__CURSOR"`),
	[]byte(`"__MONOTONIC_TIMESTAMP"`),
}

func (p *journaldParser) Detect(sample [][]byte) float64 {
	var considered, matched int

	for _, line := range sample {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		considered++
		if line[0] != '{' || line[len(line)-1] != '}' {
			continue
		}
		if hasAny(line, journaldMarkers) && json.Valid(line) {
			matched++
		}
	}

	if considered == 0 {
		return 0
	}
	return float64(matched) / float64(considered)
}

func hasAny(line []byte, markers [][]byte) bool {
	for _, m := range markers {
		if bytes.Contains(line, m) {
			return true
		}
	}
	return false
}

func (p *journaldParser) Parse(line []byte) (Record, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return Record{}, ErrNoMatch
	}

	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()

	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return Record{}, ErrNoMatch
	}
	if !hasAny(line, journaldMarkers) {
		// Valid JSON, but not a journal entry. Leaving it to jsonl is better
		// than claiming it: a mixed directory is exactly where a parser that
		// over-reaches does its damage.
		return Record{}, ErrNoMatch
	}

	rec := Record{Fields: make(map[string]any, len(raw))}

	for key, val := range raw {
		switch key {
		case "__REALTIME_TIMESTAMP":
			if ts, ok := journalTime(val); ok {
				// An epoch instant carries its own zone by construction: there
				// is no local reading of it to assume.
				rec.Timestamp, rec.TimestampZoned = ts, true
				continue
			}
		case "PRIORITY":
			if sev, ok := journalPriority(val); ok {
				rec.Level = severityLevel(sev)
				// The numeric priority is kept as well as the word. It is what
				// a systemd user filters on, and dropping it to store the
				// translation would lose the form they know.
				rec.Fields["priority"] = int64(sev)
				continue
			}
		case "MESSAGE":
			if msg, ok := journalMessage(val); ok {
				rec.Message = msg
				continue
			}
		}
		rec.Fields[key] = normaliseValue(val)
	}

	return rec, nil
}

// journalTime reads __REALTIME_TIMESTAMP, which is microseconds since the epoch
// written as a string.
//
// A string rather than a number because the value exceeds what JSON's double
// can hold exactly — 1.75e15 microseconds needs 51 bits — and systemd is right
// to quote it. Parsing it as an integer keeps that exactness.
func journalTime(val any) (time.Time, bool) {
	s, ok := coerceString(val)
	if !ok {
		return time.Time{}, false
	}

	micros, err := strconv.ParseInt(s, 10, 64)
	if err != nil || micros <= 0 {
		return time.Time{}, false
	}
	return time.UnixMicro(micros).UTC(), true
}

// journalPriority reads PRIORITY, a syslog severity digit written as a string.
func journalPriority(val any) (int, bool) {
	s, ok := coerceString(val)
	if !ok {
		return 0, false
	}

	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 7 {
		return 0, false
	}
	return n, true
}

// journalMessage reads MESSAGE, which is a string unless the message contained
// bytes JSON cannot hold, in which case systemd exports an array of byte
// values.
//
// The array form is rare and is exactly the case worth handling: it is how a
// message with an embedded NUL or invalid UTF-8 arrives, and those are the
// lines somebody is hunting for when the tool has otherwise let them down.
func journalMessage(val any) (string, bool) {
	switch v := val.(type) {
	case string:
		return v, true

	case []any:
		out := make([]byte, 0, len(v))
		for _, item := range v {
			n, ok := item.(json.Number)
			if !ok {
				return "", false
			}
			b, err := strconv.ParseUint(n.String(), 10, 8)
			if err != nil {
				return "", false
			}
			out = append(out, byte(b))
		}
		return string(out), true
	}
	return "", false
}
