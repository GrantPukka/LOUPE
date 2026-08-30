package parse

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func init() { Register(&syslogParser{}) }

// syslogParser reads both syslog wire formats.
//
// RFC5424:
//
//	<14>1 2026-08-13T14:02:00Z host-01 sshd 3344 - - session opened for user deploy
//	<PRI>V TIMESTAMP                HOST    APP  PID  MSGID SD MSG
//
// RFC3164, the BSD format everything still writes:
//
//	Aug 13 14:02:00 host-01 sshd[3344]: session opened for user deploy
//	<14>Aug 13 14:02:00 host-01 sshd[3344]: session opened for user deploy
//
// The priority encodes both facility and severity, which is where the level
// comes from — this is the one format in the package that carries a real
// severity in a machine-readable form rather than as a word. RFC3164 makes the
// priority optional, and a file written by a local daemon usually has none, so
// there the level is read out of the message text instead.
type syslogParser struct{}

func (p *syslogParser) Name() string { return "syslog" }

var syslogRe = regexp.MustCompile(
	`^<(\d{1,3})>(\d) (\S+) (\S+) (\S+) (\S+) (\S+) (?:(\[.*?\]|-) )?(.*)$`)

// syslog3164Re matches the BSD format: an optional priority, a month-day-time
// with no year, a hostname, and an optional `tag[pid]:` before the message.
//
// The tag is optional because the format permits a bare message, and it is
// matched without a colon inside it so that a payload like `CEF:0|Acme|…`
// stays whole in the message rather than being sliced into a tag.
var syslog3164Re = regexp.MustCompile(
	`^(?:<(\d{1,3})>)?([A-Z][a-z]{2} [ 0-9]\d \d{2}:\d{2}:\d{2}) (\S+) ` +
		`(?:([^\s\[\]:]+)(?:\[(\d+)\])?: )?(.*)$`)

// RFC3164 is a frame, not a payload format: haproxy, auditd and CEF all travel
// inside one, and a parser that understands what is inside the frame knows
// strictly more than this one does. It therefore claims the frame ceiling, so
// those parsers can win outright. RFC5424 is not capped: it carries its own
// structured data and needs no payload parser behind it.
const rfc3164Ceiling = frameCeiling

func (p *syslogParser) Detect(sample [][]byte) float64 {
	if len(sample) == 0 {
		return 0
	}

	var considered, matched5424, matched3164 int
	for _, line := range sample {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		considered++
		switch {
		case syslogRe.Match(line):
			matched5424++
		case syslog3164Re.Match(line):
			matched3164++
		}
	}
	if considered == 0 {
		return 0
	}

	// The two formats are scored separately so that a file of BSD lines is
	// still capped at the frame ceiling while a file of RFC5424 lines, which
	// carries its own structured data and needs no payload parser, is not.
	return (float64(matched5424) + rfc3164Ceiling*float64(matched3164)) / float64(considered)
}

func (p *syslogParser) Parse(line []byte) (Record, error) {
	m := syslogRe.FindSubmatch(line)
	if m == nil {
		return p.parse3164(line)
	}

	rec := Record{Fields: make(map[string]any, 6)}

	if pri, err := strconv.Atoi(string(m[1])); err == nil {
		rec.Level = severityLevel(pri % 8)
		rec.Fields["facility"] = int64(pri / 8)
		rec.Fields["severity"] = int64(pri % 8)
	}

	if version := string(m[2]); version != "1" {
		// Only version 1 is defined, but recording an unexpected one is better
		// than discarding it.
		rec.Fields["syslog_version"] = version
	}

	if ts, zoned, ok := ParseTime(string(m[3]), time.UTC); ok {
		rec.Timestamp, rec.TimestampZoned = ts, zoned
	}

	// A nil value in RFC5424 is written as a bare hyphen. Storing that would
	// be storing the absence of data as data.
	setIfPresent(rec.Fields, "host", string(m[4]))
	setIfPresent(rec.Fields, "app", string(m[5]))
	setIfPresent(rec.Fields, "pid", string(m[6]))
	setIfPresent(rec.Fields, "msgid", string(m[7]))
	parseStructuredData(string(m[8]), rec.Fields)

	// RFC5424 permits a UTF-8 byte order mark before the message, which is
	// invisible in a terminal and would otherwise corrupt a prefix search.
	rec.Message = strings.TrimPrefix(string(m[9]), "\ufeff")

	return rec, nil
}

// parse3164 reads the BSD format.
//
// The year is absent from the wire, so the timestamp is reported as unzoned
// and normaliseYear fills the year in. Both are assumptions, and both are
// disclosed by `loupe sources` rather than being applied quietly.
func (p *syslogParser) parse3164(line []byte) (Record, error) {
	m := syslog3164Re.FindSubmatch(line)
	if m == nil {
		return Record{}, ErrNoMatch
	}

	rec := Record{Fields: make(map[string]any, 8)}

	if pri := string(m[1]); pri != "" {
		if n, err := strconv.Atoi(pri); err == nil {
			rec.Level = severityLevel(n % 8)
			rec.Fields["facility"] = int64(n / 8)
			rec.Fields["severity"] = int64(n % 8)
		}
	}

	if ts, _, ok := ParseTime(string(m[2]), time.UTC); ok {
		// RFC3164 writes no offset at all, so this is a wall clock in the
		// source's assumed zone whatever the priority said.
		rec.Timestamp, rec.TimestampZoned = ts, false
	}

	setIfPresent(rec.Fields, "host", string(m[3]))
	setIfPresent(rec.Fields, "app", string(m[4]))
	setIfPresent(rec.Fields, "pid", string(m[5]))

	rec.Message = string(m[6])

	// auditd, and any daemon that logs key=value through syslog, puts the whole
	// record in the message. Leaving it there means approved_by=NONE is text a
	// person has to eyeball rather than a field they can filter on, which is the
	// difference between finding an unapproved role change and scrolling past it.
	addKeyValueMessage(rec.Fields, m[6])

	if rec.Level == "" {
		rec.Level = levelFromMessage(rec.Message)
	}

	return rec, nil
}

// sdElementRe matches one RFC5424 structured-data element:
// [id@enterprise key="value" key="value"].
var sdElementRe = regexp.MustCompile(`\[([^\s\]]+)((?:\s+[^\s=\]]+="(?:[^"\\]|\\.)*")*)\s*\]`)

// sdParamRe matches one key="value" pair inside an element.
var sdParamRe = regexp.MustCompile(`([^\s=\]]+)="((?:[^"\\]|\\.)*)"`)

// parseStructuredData promotes RFC5424 structured data into real fields.
//
// This is the one place in syslog where an application can attach named values
// — a trace id, a request id, a tenant — and leaving it as an opaque string
// would mean trace_id:a91c40f2 cannot reach a syslog source at all. Parsing it
// is reading the format as specified, not guessing.
//
// The element id is kept too, since it names the schema the parameters belong
// to, and a parameter name that would collide with an already-extracted field
// is prefixed rather than allowed to overwrite it.
func parseStructuredData(sd string, fields map[string]any) {
	if sd == "" || sd == "-" {
		return
	}

	elements := sdElementRe.FindAllStringSubmatch(sd, -1)
	if len(elements) == 0 {
		// Unparseable structured data is still data. Keep it whole rather than
		// dropping it.
		fields["structured_data"] = sd
		return
	}

	for _, element := range elements {
		id := element[1]
		if _, taken := fields["sd_id"]; !taken {
			fields["sd_id"] = id
		}

		for _, param := range sdParamRe.FindAllStringSubmatch(element[2], -1) {
			key, value := param[1], unescapeSDValue(param[2])
			if _, taken := fields[key]; taken {
				key = id + "." + key
			}
			setIfPresent(fields, key, value)
		}
	}
}

// unescapeSDValue undoes the RFC5424 escaping of ", \, and ].
func unescapeSDValue(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	r := strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\]`, `]`)
	return r.Replace(s)
}

func setIfPresent(fields map[string]any, key, value string) {
	if value == "" || value == "-" {
		return
	}
	// Numeric identifiers such as pid are more useful as numbers, so a filter
	// like pid:>20000 compares numerically.
	if n, err := strconv.ParseInt(value, 10, 64); err == nil && !strings.HasPrefix(value, "0") {
		fields[key] = n
		return
	}
	fields[key] = value
}

// severityLevel maps an RFC5424 severity onto the canonical set.
//
// Syslog has eight levels to our six. Emergency, alert, and critical all mean
// "worse than an error", which is what fatal means here, and notice is
// conventionally informational.
func severityLevel(severity int) string {
	switch severity {
	case 0, 1, 2: // emergency, alert, critical
		return LevelFatal
	case 3:
		return LevelError
	case 4:
		return LevelWarn
	case 5, 6: // notice, informational
		return LevelInfo
	case 7:
		return LevelDebug
	default:
		return ""
	}
}
