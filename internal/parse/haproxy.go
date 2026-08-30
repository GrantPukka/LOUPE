package parse

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func init() { Register(&haproxyParser{}) }

// haproxyParser reads the HAProxy HTTP log.
//
//	Aug 20 21:00:15 lb-01 haproxy[1982]: 10.0.0.1:36451 [20/Aug/2026:21:00:15.473] fe_https be_app/app-03 31/66/404/701/767 200 49529 - - ---- 652/3869/340/110/1 0/0 "GET /c.css HTTP/1.1"
//
// HAProxy writes through syslog, so the line arrives inside an RFC3164 frame.
// This parser reads the frame itself rather than leaving the payload as an
// opaque message: without it the busiest source at the edge of a platform has
// no status field, and `status:>=500` — the one filter someone types during a
// 502 storm — returns nothing on the load balancer's own log.
//
// The frame is optional because HAProxy can also log to a file directly.
type haproxyParser struct{}

func (p *haproxyParser) Name() string { return "haproxy" }

var haproxyRe = regexp.MustCompile(
	// optional RFC3164 frame
	`^(?:(?:<\d{1,3}>)?[A-Z][a-z]{2} [ 0-9]\d \d{2}:\d{2}:\d{2} (\S+) \S+?(?:\[(\d+)\])?: )?` +
		// client:port [accept_date] frontend backend/server
		`(\S+):(\d+) \[([^\]]+)\] (\S+) (\S+?)/(\S+) ` +
		// Tq/Tw/Tc/Tr/Tt  status  bytes
		`(-?\d+)/(-?\d+)/(-?\d+)/(-?\d+)/(\+?-?\d+) (\d{3}) (\+?\d+|-) ` +
		// captured cookies, termination state
		`(\S+) (\S+) (\S{4}) ` +
		// actconn/feconn/beconn/srv_conn/retries  srv_queue/backend_queue
		`(\d+)/(\d+)/(\d+)/(\d+)/(\+?\d+) (\d+)/(\d+)` +
		`(?: "([^"]*)")?`)

// haproxyTimers names the five durations in the Tq/Tw/Tc/Tr/Tt tuple, in
// order. HAProxy's own documentation uses these names, and a tuple of five
// bare integers is unreadable without them: Tw is the queue wait and Tr is the
// server's response time, and telling those apart is the whole diagnosis when
// a backend is saturated.
var haproxyTimers = []string{
	"tq_ms", "tw_ms", "tc_ms", "tr_ms", "tt_ms",
}

// haproxyConns names the actconn/feconn/beconn/srv_conn/retries tuple.
var haproxyConns = []string{
	"actconn", "feconn", "beconn", "srv_conn", "retries",
}

// haproxyAcceptLayouts are the accept-date spellings, with and without the
// offset HAProxy omits when its syslog frame already carries the time. They
// are kept here rather than in Layouts because the millisecond form is
// HAProxy's alone and would only slow every other format's lookup down.
var haproxyAcceptLayouts = []string{
	"02/Jan/2006:15:04:05.999 -0700",
	"02/Jan/2006:15:04:05.999",
}

func (p *haproxyParser) Detect(sample [][]byte) float64 {
	if len(sample) == 0 {
		return 0
	}

	var considered, matched int
	for _, line := range sample {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		considered++
		if haproxyRe.Match(line) {
			matched++
		}
	}
	if considered == 0 {
		return 0
	}
	return float64(matched) / float64(considered)
}

func (p *haproxyParser) Parse(line []byte) (Record, error) {
	m := haproxyRe.FindSubmatch(line)
	if m == nil {
		return Record{}, ErrNoMatch
	}

	rec := Record{Fields: make(map[string]any, 20)}

	rec.Timestamp, rec.TimestampZoned = haproxyAcceptTime(string(m[5]))

	setIfPresent(rec.Fields, "host", string(m[1]))
	setIfPresent(rec.Fields, "pid", string(m[2]))
	rec.Fields["client"] = string(m[3])
	setIfPresent(rec.Fields, "client_port", string(m[4]))
	rec.Fields["frontend"] = string(m[6])
	rec.Fields["backend"] = string(m[7])
	setIfPresent(rec.Fields, "server", string(m[8]))

	for i, name := range haproxyTimers {
		setIfPresent(rec.Fields, name, string(m[9+i]))
	}

	if status, err := strconv.ParseInt(string(m[14]), 10, 64); err == nil {
		rec.Fields["status"] = status
		rec.Level = levelForStatus(status)
	}
	if sent := strings.TrimPrefix(string(m[15]), "+"); sent != "-" {
		setIfPresent(rec.Fields, "bytes", sent)
	}

	// The four-character termination state is HAProxy's account of how the
	// session ended — sC means the server refused the connection, sH a server
	// timeout during the header. It is the field that says whether a 502 was
	// the backend's fault or the client's, so it is stored as written rather
	// than decoded into a guess.
	rec.Fields["term_state"] = string(m[18])

	for i, name := range haproxyConns {
		setIfPresent(rec.Fields, name, string(m[19+i]))
	}
	setIfPresent(rec.Fields, "srv_queue", string(m[24]))
	setIfPresent(rec.Fields, "backend_queue", string(m[25]))

	request := string(m[26])
	rec.Message = request
	if rm := requestRe.FindStringSubmatch(request); rm != nil {
		rec.Fields["method"] = rm[1]
		rec.Fields["path"] = rm[2]
		if rm[3] != "" {
			rec.Fields["protocol"] = rm[3]
		}
		rec.Message = rm[1] + " " + rm[2]
	}

	return rec, nil
}

// haproxyAcceptTime reads the bracketed accept date.
//
// HAProxy writes the offset when it logs to a file and omits it when the
// syslog frame around it already carries the time. An omitted offset is
// reported as zoneless so the source's assumed zone applies and is disclosed,
// rather than being quietly read as UTC.
func haproxyAcceptTime(stamp string) (time.Time, bool) {
	for _, layout := range haproxyAcceptLayouts {
		zoned := strings.HasSuffix(layout, "-0700")
		if zoned {
			if t, err := time.Parse(layout, stamp); err == nil {
				return t, true
			}
			continue
		}
		if t, err := time.ParseInLocation(layout, stamp, time.UTC); err == nil {
			return t, false
		}
	}
	if t, zoned, ok := ParseTime(stamp, time.UTC); ok {
		return t, zoned
	}
	return time.Time{}, false
}
