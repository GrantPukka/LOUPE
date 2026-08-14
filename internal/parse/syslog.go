package parse

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func init() { Register(&syslogParser{}) }

// syslogParser reads RFC5424 syslog.
//
//	<14>1 2026-08-13T14:02:00Z host-01 sshd 3344 - - session opened for user deploy
//	<PRI>V TIMESTAMP                HOST    APP  PID  MSGID SD MSG
//
// The priority encodes both facility and severity, which is where the level
// comes from — this is the one format in the package that carries a real
// severity in a machine-readable form rather than as a word.
type syslogParser struct{}

func (p *syslogParser) Name() string { return "syslog" }

var syslogRe = regexp.MustCompile(
	`^<(\d{1,3})>(\d) (\S+) (\S+) (\S+) (\S+) (\S+) (?:(\[.*?\]|-) )?(.*)$`)

func (p *syslogParser) Detect(sample [][]byte) float64 {
	if len(sample) == 0 {
		return 0
	}

	var considered, matched int
	for _, line := range sample {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		considered++
		if syslogRe.Match(line) {
			matched++
		}
	}
	if considered == 0 {
		return 0
	}
	return float64(matched) / float64(considered)
}

func (p *syslogParser) Parse(line []byte) (Record, error) {
	m := syslogRe.FindSubmatch(line)
	if m == nil {
		return Record{}, ErrNoMatch
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
