package parse

// logfmt is the clearest example of a parser in this package, and CONTRIBUTING.md
// points new contributors at it as the template. Keep it readable.

import (
	"bytes"
	"strconv"
	"strings"
	"time"
)

func init() { Register(&logfmtParser{}) }

type logfmtParser struct{}

func (p *logfmtParser) Name() string { return "logfmt" }

// Detect looks for the key=value shape. It is deliberately conservative: a
// prose line containing one equals sign should not be claimed as logfmt, so a
// line only counts when it carries at least two well-formed pairs.
//
// It does not require the *first* token to be a pair. Requiring that rejected
// every line written as `<timestamp> level=info svc=… msg=…`, which is what a
// bare-timestamp logfmt writer and every metrics agent emit, and left those
// lines to no parser at all.
//
// The result is capped at genericKVCeil: a key=value tail is something most
// formats have, so a parser that recognises the whole line has to be able to
// outrank this one. See the ceiling ladder in detect.go.
func (p *logfmtParser) Detect(sample [][]byte) float64 {
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

		// JSON is full of colons and quotes but is not logfmt; let the jsonl
		// parser have it rather than tying on a malformed-looking match.
		if line[0] == '{' || line[0] == '[' {
			continue
		}

		if keyedPairs(parseLogfmt(line)) >= 2 {
			matched++
		}
	}
	if considered == 0 {
		return 0
	}
	return genericKVCeil * float64(matched) / float64(considered)
}

// keyedPairs counts the pairs that actually carry a key.
func keyedPairs(pairs []logfmtPair) int {
	var n int
	for _, pair := range pairs {
		if pair.key != "" {
			n++
		}
	}
	return n
}

func (p *logfmtParser) Parse(line []byte) (Record, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return Record{}, ErrNoMatch
	}

	pairs := parseLogfmt(line)
	if len(pairs) == 0 {
		return Record{}, ErrNoMatch
	}

	// A line of nothing but bare words is not logfmt, whatever else it is.
	// Claiming it would mark genuinely damaged lines as parsed and hide them
	// from the unparsed count, which is the number that tells a user their data
	// is incomplete.
	if keyedPairs(pairs) == 0 {
		return Record{}, ErrNoMatch
	}

	rec := Record{Fields: make(map[string]any, len(pairs))}
	var bare []string

	for _, pair := range pairs {
		// A token with no equals sign is a bare word. Keeping it as part of the
		// message is better than discarding it.
		if pair.key == "" {
			bare = append(bare, pair.value)
			continue
		}

		key := strings.ToLower(pair.key)
		switch {
		case rec.Timestamp.IsZero() && isKey(key, tsKeys):
			if ts, zoned, ok := ParseTime(pair.value, time.UTC); ok {
				rec.Timestamp, rec.TimestampZoned = ts, zoned
				continue
			}
		case rec.Level == "" && isKey(key, levelKeys):
			rec.Level = NormaliseLevel(pair.value)
			continue
		case rec.Message == "" && isKey(key, msgKeys):
			rec.Message = pair.value
			continue
		}
		putField(rec.Fields, pair.key, typed(pair.value))
	}

	// A leading bare token is very often the timestamp: Go's own slog text
	// handler writes `time=`, but a metrics agent writes a bare epoch float and
	// plenty of services write a bare RFC3339 stamp before the first pair. It is
	// offered to ParseTime only in first position and only when no keyed
	// timestamp was found, so a message that happens to begin with a number is
	// not turned into a date.
	if rec.Timestamp.IsZero() && len(bare) > 0 && pairs[0].key == "" {
		if ts, zoned, ok := ParseTime(bare[0], time.UTC); ok {
			rec.Timestamp, rec.TimestampZoned = ts, zoned
			bare = bare[1:]
		}
	}

	if rec.Message == "" && len(bare) > 0 {
		rec.Message = strings.Join(bare, " ")
	}

	return rec, nil
}

type logfmtPair struct {
	key   string
	value string
}

// parseLogfmt splits a line into key=value pairs, honouring double-quoted
// values with backslash escapes. A token with no equals sign is returned with
// an empty key rather than being dropped.
func parseLogfmt(line []byte) []logfmtPair {
	var pairs []logfmtPair
	i, n := 0, len(line)

	for i < n {
		for i < n && line[i] == ' ' {
			i++
		}
		if i >= n {
			break
		}

		keyStart := i
		for i < n && line[i] != '=' && line[i] != ' ' {
			i++
		}

		// No equals sign before the next space, so this is a bare token.
		if i >= n || line[i] == ' ' {
			pairs = append(pairs, logfmtPair{value: string(line[keyStart:i])})
			continue
		}

		key := string(line[keyStart:i])
		i++ // skip '='

		value, next := readLogfmtValue(line, i)
		i = next
		pairs = append(pairs, logfmtPair{key: key, value: value})
	}

	return pairs
}

// readLogfmtValue reads one value starting at i, returning it and the index
// just past it.
func readLogfmtValue(line []byte, i int) (string, int) {
	n := len(line)
	if i >= n {
		return "", i
	}

	if line[i] != '"' {
		start := i
		for i < n && line[i] != ' ' {
			i++
		}
		return string(line[start:i]), i
	}

	i++ // opening quote
	var sb strings.Builder
	for i < n {
		switch line[i] {
		case '\\':
			// A trailing backslash is damage, not an escape; keep it literally.
			if i+1 >= n {
				sb.WriteByte('\\')
				return sb.String(), n
			}
			switch line[i+1] {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			default:
				sb.WriteByte(line[i+1])
			}
			i += 2
		case '"':
			return sb.String(), i + 1
		default:
			sb.WriteByte(line[i])
			i++
		}
	}

	// Unterminated quote, which happens on a truncated line. Return what there
	// is rather than failing the record.
	return sb.String(), n
}

// typed converts a logfmt value, which is always text on the wire, into a
// concrete type so that numeric comparison works in filters.
//
// Leading zeros are kept as strings: an ID like 007 is not the number 7, and
// silently changing it would break an equality filter.
func typed(s string) any {
	if s == "" {
		return ""
	}

	if len(s) > 1 && s[0] == '0' && s[1] != '.' {
		return s
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	switch s {
	case "true":
		return true
	case "false":
		return false
	}
	return s
}
