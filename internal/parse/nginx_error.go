package parse

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func init() { Register(&nginxErrorParser{}) }

// nginxErrorParser reads the Nginx error log, which shares nothing with the
// access log but the name.
//
//	2026/08/20 21:00:00 [crit] 9234#4: *4064898 SSL_do_handshake() failed while SSL handshaking, client: 203.148.230.196
//	2026/08/20 21:00:07 [error] 4075#0: *9105870 connect() failed (111: Connection refused) while connecting to upstream, client: 154.2.3.4, upstream: "http://app-01:8080/"
//
// It is a separate parser rather than a second pattern inside nginxParser
// because it is a separate format with a separate severity vocabulary, and
// because `format:nginx_error` is a question people ask during an incident:
// the access log says a request 502'd, and this log says why.
//
// The timestamp carries no offset, so records are reported as zoneless and the
// source's assumed zone applies.
type nginxErrorParser struct{}

func (p *nginxErrorParser) Name() string { return "nginx_error" }

var nginxErrorRe = regexp.MustCompile(
	`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}) \[(\w+)\] (\d+)#(\d+): (?:\*(\d+) )?(.*)$`)

// nginxErrorContextRe pulls the trailing `key: value` context nginx appends to
// an error: client, server, request, upstream, host, referrer.
//
// It is a fixed vocabulary rather than "anything before a colon" because the
// message itself is full of colons — `(111: Connection refused)` is part of the
// error text, not a field.
var nginxErrorContextRe = regexp.MustCompile(
	`,\s*(client|server|request|upstream|host|referrer|subrequest):\s*("[^"]*"|[^,]*)`)

func (p *nginxErrorParser) Detect(sample [][]byte) float64 {
	if len(sample) == 0 {
		return 0
	}

	var considered, matched int
	for _, line := range sample {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		considered++
		if nginxErrorRe.Match(line) {
			matched++
		}
	}
	if considered == 0 {
		return 0
	}
	return float64(matched) / float64(considered)
}

func (p *nginxErrorParser) Parse(line []byte) (Record, error) {
	m := nginxErrorRe.FindSubmatch(line)
	if m == nil {
		return Record{}, ErrNoMatch
	}

	rec := Record{
		Level:  NormaliseLevel(string(m[2])),
		Fields: make(map[string]any, 8),
	}

	if ts, _, ok := ParseTime(string(m[1]), time.UTC); ok {
		rec.Timestamp, rec.TimestampZoned = ts, false
	}

	if pid, err := strconv.ParseInt(string(m[3]), 10, 64); err == nil {
		rec.Fields["pid"] = pid
	}
	if tid, err := strconv.ParseInt(string(m[4]), 10, 64); err == nil {
		rec.Fields["tid"] = tid
	}
	if conn := string(m[5]); conn != "" {
		if n, err := strconv.ParseInt(conn, 10, 64); err == nil {
			rec.Fields["connection"] = n
		}
	}

	message := string(m[6])
	for _, c := range nginxErrorContextRe.FindAllStringSubmatch(message, -1) {
		putField(rec.Fields, c[1], typed(strings.Trim(strings.TrimSpace(c[2]), `"`)))
	}
	// The context is metadata that every line repeats; the message is the part
	// that differs, and it is what `patterns` should be templating.
	if i := nginxErrorContextRe.FindStringIndex(message); i != nil {
		message = message[:i[0]]
	}
	rec.Message = strings.TrimSpace(message)

	return rec, nil
}
