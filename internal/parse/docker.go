package parse

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

func init() { Register(&dockerParser{}) }

// dockerParser reads Docker's json-file logging driver, the format under
// /var/lib/docker/containers/<id>/<id>-json.log.
//
// Each line is a JSON object of exactly three keys — log, stream, time — plus
// an optional attrs map from --log-opt labels. jsonl would half-read it: time
// and log are in its key lists, so it would find the timestamp and the message
// and then leave the trailing newline on every one of them. The half that
// matters is the half it gets wrong.
type dockerParser struct{}

func (p *dockerParser) Name() string { return "docker" }

func (p *dockerParser) Detect(sample [][]byte) float64 {
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

		var entry dockerEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		// All three keys, because two of them are ordinary names that any
		// number of application logs use. The trio together is the signature.
		if entry.Stream != "" && entry.Time != "" && entry.Log != nil {
			matched++
		}
	}

	if considered == 0 {
		return 0
	}
	return float64(matched) / float64(considered)
}

// dockerEntry is the json-file record. Log is a pointer so that an empty
// message can be told apart from an absent key, which is what stops a bare
// {"stream":"stdout","time":"..."} being claimed.
type dockerEntry struct {
	Log    *string         `json:"log"`
	Stream string          `json:"stream"`
	Time   string          `json:"time"`
	Attrs  map[string]any  `json:"attrs,omitempty"`
	Extra  json.RawMessage `json:"-"`
}

func (p *dockerParser) Parse(line []byte) (Record, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return Record{}, ErrNoMatch
	}

	var entry dockerEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		return Record{}, ErrNoMatch
	}
	if entry.Log == nil || entry.Stream == "" {
		return Record{}, ErrNoMatch
	}

	rec := Record{Fields: map[string]any{"stream": entry.Stream}}

	if ts, zoned, ok := ParseTime(entry.Time, time.UTC); ok {
		rec.Timestamp, rec.TimestampZoned = ts, zoned
	}

	// The driver appends the newline the process wrote. Keeping it would put a
	// blank line under every record in the table and a stray \n in every
	// handoff.
	rec.Message = strings.TrimRight(*entry.Log, "\r\n")

	// Docker carries no severity of its own, so it is read out of the message
	// by the same rule the fallback parser uses. Guessing from the stream would
	// be worse than not guessing: plenty of programs write ordinary progress to
	// stderr, and marking all of it error-level would poison level:>=error for
	// the whole source.
	rec.Level = levelFromMessage(rec.Message)

	// Labels from --log-opt travel under attrs. They are flattened rather than
	// nested so that a filter can name one directly.
	for key, val := range entry.Attrs {
		rec.Fields[key] = normaliseValue(val)
	}

	return rec, nil
}
