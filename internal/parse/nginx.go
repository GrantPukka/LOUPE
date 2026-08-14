package parse

import (
	"bytes"
	"regexp"
	"strconv"
	"time"
)

func init() { Register(&nginxParser{}) }

// nginxParser reads the Nginx and Apache access log formats.
//
// Combined:
//
//	10.0.0.1 - alice [13/Aug/2026:14:02:00 +0000] "POST /api/cart HTTP/1.1" 200 547 "-" "curl/8.4.0"
//
// Common is the same without the trailing referer and user-agent. Both are
// handled by one expression, since the difference is two optional groups.
type nginxParser struct{}

func (p *nginxParser) Name() string { return "nginx" }

// nginxRe matches common and combined in one pass.
//
// The bracketed timestamp is the distinctive part: no other format in this
// package writes a date that way, which is what makes detection reliable.
var nginxRe = regexp.MustCompile(
	`^(\S+) (\S+) (\S+) \[([^\]]+)\] "([^"]*)" (\d{3}) (\d+|-)` +
		`(?: "([^"]*)" "([^"]*)")?`)

// requestRe splits the quoted request line into method, path, and protocol.
//
// It is separate because a malformed request line is common — scanners send
// junk — and a request that does not split must not fail the whole record.
var requestRe = regexp.MustCompile(`^(\S+) (\S+)(?: (\S+))?$`)

func (p *nginxParser) Detect(sample [][]byte) float64 {
	if len(sample) == 0 {
		return 0
	}

	var considered, matched int
	for _, line := range sample {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		considered++
		if nginxRe.Match(line) {
			matched++
		}
	}
	if considered == 0 {
		return 0
	}
	return float64(matched) / float64(considered)
}

func (p *nginxParser) Parse(line []byte) (Record, error) {
	m := nginxRe.FindSubmatch(line)
	if m == nil {
		return Record{}, ErrNoMatch
	}

	rec := Record{Fields: make(map[string]any, 8)}

	// The bracketed time carries its own offset, so the zone is known.
	if ts, zoned, ok := ParseTime(string(m[4]), time.UTC); ok {
		rec.Timestamp, rec.TimestampZoned = ts, zoned
	}

	rec.Fields["client"] = string(m[1])
	if user := string(m[3]); user != "-" {
		rec.Fields["user"] = user
	}

	request := string(m[5])
	rec.Message = request
	if rm := requestRe.FindStringSubmatch(request); rm != nil {
		rec.Fields["method"] = rm[1]
		rec.Fields["path"] = rm[2]
		if rm[3] != "" {
			rec.Fields["protocol"] = rm[3]
		}
		// The path alone reads better in a timeline than the full request
		// line, and the method is one column away.
		rec.Message = rm[1] + " " + rm[2]
	}

	status, err := strconv.ParseInt(string(m[6]), 10, 64)
	if err == nil {
		rec.Fields["status"] = status
		rec.Level = levelForStatus(status)
	}

	if bytesSent := string(m[7]); bytesSent != "-" {
		if n, err := strconv.ParseInt(bytesSent, 10, 64); err == nil {
			rec.Fields["bytes"] = n
		}
	}

	if referer := string(m[8]); referer != "" && referer != "-" {
		rec.Fields["referer"] = referer
	}
	if agent := string(m[9]); agent != "" && agent != "-" {
		rec.Fields["agent"] = agent
	}

	return rec, nil
}

// levelForStatus derives a severity from the HTTP status code.
//
// Access logs carry no severity field, so this is inferred rather than read.
// It is worth doing: the whole cross-source pitch is watching a database fail,
// then the app error, then Nginx return 502s, and that only works if
// level:>=error catches all three. Without it a user has to know to write
// status:>=500 for one source and level:>=error for the others.
//
// The mapping is the conventional one and nothing else is guessed at: a 2xx or
// 3xx is info, not "success", and no level is invented for a status the
// expression did not capture.
func levelForStatus(status int64) string {
	switch {
	case status >= 500:
		return LevelError
	case status >= 400:
		return LevelWarn
	default:
		return LevelInfo
	}
}
