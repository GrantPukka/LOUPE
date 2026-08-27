package parse

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func init() { Register(&cefParser{}) }

// cefParser reads ArcSight Common Event Format, which firewalls, IDS and most
// security appliances speak.
//
//	<134>Aug 20 21:00:46 fw01 CEF:0|Acme|EdgeFirewall|9.2|902|Malformed packet dropped|5|src=1.2.3.4 dst=5.6.7.8 spt=52801 dpt=5432 act=blocked
//
// The header is seven pipe-delimited fields, then an extension of key=value
// pairs. It travels inside an RFC3164 syslog frame, which is read here so the
// record gets a timestamp and a host: the syslog parser alone would leave the
// whole CEF payload as message text, and `act:blocked` or `dst:5.6.7.8` — the
// only two things anybody asks a firewall log — would not be fields at all.
//
// The pipe delimiter is why this is a parser rather than a key=value tail on
// syslog: splitting `…|Malformed packet dropped|5|src=1.2.3.4` on whitespace
// invents a field named `dropped|5|src`.
type cefParser struct{}

func (p *cefParser) Name() string { return "cef" }

var cefRe = regexp.MustCompile(
	`^(?:(?:<(\d{1,3})>)?([A-Z][a-z]{2} [ 0-9]\d \d{2}:\d{2}:\d{2}) (\S+) )?` +
		`CEF:(\d+)\|((?:[^|\\]|\\.)*)\|((?:[^|\\]|\\.)*)\|((?:[^|\\]|\\.)*)\|` +
		`((?:[^|\\]|\\.)*)\|((?:[^|\\]|\\.)*)\|((?:[^|\\]|\\.)*)\|(.*)$`)

// cefHeader names the pipe-delimited header fields after the version.
var cefHeader = []string{
	"cef_vendor", "cef_product", "cef_version",
	"cef_signature", "cef_name", "cef_severity",
}

func (p *cefParser) Detect(sample [][]byte) float64 {
	if len(sample) == 0 {
		return 0
	}

	var considered, matched int
	for _, line := range sample {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		considered++
		if cefRe.Match(line) {
			matched++
		}
	}
	if considered == 0 {
		return 0
	}
	return float64(matched) / float64(considered)
}

func (p *cefParser) Parse(line []byte) (Record, error) {
	m := cefRe.FindSubmatch(line)
	if m == nil {
		return Record{}, ErrNoMatch
	}

	rec := Record{Fields: make(map[string]any, 16)}

	if pri := string(m[1]); pri != "" {
		if n, err := strconv.Atoi(pri); err == nil {
			rec.Level = severityLevel(n % 8)
			rec.Fields["facility"] = int64(n / 8)
		}
	}
	if stamp := string(m[2]); stamp != "" {
		if ts, _, ok := ParseTime(stamp, time.UTC); ok {
			// RFC3164 carries no offset, so the source's assumed zone applies.
			rec.Timestamp, rec.TimestampZoned = ts, false
		}
	}
	setIfPresent(rec.Fields, "host", string(m[3]))
	setIfPresent(rec.Fields, "cef_format_version", string(m[4]))

	for i, name := range cefHeader {
		setIfPresent(rec.Fields, name, cefUnescape(string(m[5+i])))
	}

	// The name is what the appliance called the event, which reads far better
	// in a timeline than the extension does.
	rec.Message = cefUnescape(string(m[9]))

	// CEF severity is 0-10, unrelated to the syslog priority and often the only
	// severity present. It is mapped only when the frame gave no level, so a
	// real priority is never overridden by a derived one.
	if rec.Level == "" {
		rec.Level = cefSeverityLevel(string(m[10]))
	}

	// The extension is genuine key=value and needs no minimum: a CEF line that
	// got this far is CEF, whatever its extension looks like.
	addKeyValueTail(rec.Fields, m[11])

	return rec, nil
}

// cefSeverityLevel maps CEF's 0-10 scale onto the canonical levels. The bands
// are the ones the specification itself names: 0-3 low, 4-6 medium, 7-8 high,
// 9-10 very high.
func cefSeverityLevel(s string) string {
	n, err := strconv.Atoi(s)
	if err != nil {
		return ""
	}
	switch {
	case n >= 9:
		return LevelFatal
	case n >= 7:
		return LevelError
	case n >= 4:
		return LevelWarn
	default:
		return LevelInfo
	}
}

// cefUnescape undoes the header escaping of | and \.
func cefUnescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	return strings.NewReplacer(`\|`, `|`, `\\`, `\`).Replace(s)
}
