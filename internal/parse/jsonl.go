package parse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

func init() { Register(&jsonlParser{}) }

type jsonlParser struct{}

func (p *jsonlParser) Name() string { return "jsonl" }

// tsKeys are the field names checked for a timestamp, in preference order.
var tsKeys = []string{"ts", "timestamp", "time", "@timestamp", "eventTime", "date", "t"}

// levelKeys are the field names checked for a severity, in preference order.
var levelKeys = []string{"level", "lvl", "severity", "levelname", "log.level", "loglevel"}

// msgKeys are the field names checked for the human-readable message.
var msgKeys = []string{"msg", "message", "event", "text", "log", "@message"}

func (p *jsonlParser) Detect(sample [][]byte) float64 {
	if len(sample) == 0 {
		return 0
	}

	var considered, matched int
	for _, line := range sample {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		considered++
		// A cheap shape check first: json.Valid on every line of a large sample
		// is measurably slower and the brace test rejects almost everything.
		if line[0] != '{' || line[len(line)-1] != '}' {
			continue
		}
		if json.Valid(line) {
			matched++
		}
	}
	if considered == 0 {
		return 0
	}
	return float64(matched) / float64(considered)
}

func (p *jsonlParser) Parse(line []byte) (Record, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return Record{}, ErrNoMatch
	}

	// json.Number keeps integer IDs from being mangled into float64 and
	// re-rendered in scientific notation, which silently corrupts values like
	// a 19-digit trace id.
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()

	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return Record{}, ErrNoMatch
	}

	rec := Record{Fields: make(map[string]any, len(raw))}

	for key, val := range raw {
		switch {
		case rec.Timestamp.IsZero() && isKey(key, tsKeys):
			if ts, zoned, ok := coerceTime(val); ok {
				rec.Timestamp, rec.TimestampZoned = ts, zoned
				continue
			}
			// Not a timestamp after all, so keep it as an ordinary field
			// rather than dropping it.
		case rec.Level == "" && isKey(key, levelKeys):
			if s, ok := coerceString(val); ok {
				rec.Level = NormaliseLevel(s)
				continue
			}
		case rec.Message == "" && isKey(key, msgKeys):
			if s, ok := coerceString(val); ok {
				rec.Message = s
				continue
			}
		}
		rec.Fields[key] = normaliseValue(val)
	}

	// A JSON object with none of the three recognisable shapes is more likely
	// to be some other kind of JSON than a log line. Render it as the message
	// so nothing is lost.
	if rec.Message == "" && rec.Level == "" && rec.Timestamp.IsZero() {
		rec.Message = string(line)
	}

	return rec, nil
}

// isKey compares case-insensitively, since JSON loggers disagree about
// capitalisation and a Level field should not become an unpromoted one.
func isKey(key string, candidates []string) bool {
	for _, c := range candidates {
		if len(key) == len(c) && equalFold(key, c) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// coerceTime accepts the string and numeric timestamp forms, reporting whether
// the value carried its own timezone.
//
// A JSON log writing "2026-08-13T14:00:00" with no offset is common, and the
// difference between that and the same instant with a Z matters by an hour.
func coerceTime(val any) (t time.Time, zoned, ok bool) {
	switch v := val.(type) {
	case string:
		return ParseTime(v, time.UTC)
	case json.Number:
		return ParseTime(v.String(), time.UTC)
	}
	return time.Time{}, false, false
}

func coerceString(val any) (string, bool) {
	switch v := val.(type) {
	case string:
		return v, true
	case json.Number:
		return v.String(), true
	case bool:
		return strconv.FormatBool(v), true
	}
	return "", false
}

// normaliseValue converts json.Number back to a concrete Go type so that the
// store can type the column, while keeping integers exact.
func normaliseValue(val any) any {
	switch v := val.(type) {
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i
		}
		if f, err := v.Float64(); err == nil {
			return f
		}
		return v.String()
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, inner := range v {
			out[k] = normaliseValue(inner)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, inner := range v {
			out[i] = normaliseValue(inner)
		}
		return out
	default:
		return v
	}
}

// String renders a value for display and for free-text search. It exists so
// that searching for a bare word matches numeric and boolean field values too,
// which is what people expect from a search box.
func String(val any) string {
	switch v := val.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case time.Time:
		return v.Format(time.RFC3339Nano)
	default:
		return fmt.Sprint(v)
	}
}
