package parse

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func init() { Register(&postgresParser{}) }

// postgresParser reads the PostgreSQL server log.
//
//	2026-08-13 14:02:00.100 UTC [20353] LOG:  duration: 178.328 ms
//	2026-08-13 14:02:00.100 UTC [20353] alice@checkout ERROR:  relation "x" does not exist
//	2026-08-13 14:02:00.100 [20353] FATAL:  remaining connection slots are reserved
//
// The zone is written as an abbreviation rather than an offset, and often not
// at all. An abbreviation is not enough to place an instant — CST is two
// different zones — so unless it is UTC the timestamp is treated as zoneless
// and the source's assumed zone applies. That assumption is then disclosed by
// `loupe sources`, which is the point.
type postgresParser struct{}

func (p *postgresParser) Name() string { return "postgres" }

var postgresRe = regexp.MustCompile(
	`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)?)(?: ([A-Z]{2,5}|[+-]\d{2}(?::?\d{2})?))?` +
		` \[(\d+)(?:-\d+)?\]` +
		`(?: ([^ ]+@[^ ]+))?` +
		` ([A-Z][A-Z0-9]*):\s*(.*)$`)

// postgresLevels are the message prefixes Postgres writes. Anything else in
// that position is not a Postgres log line, which is what keeps detection from
// claiming other formats that happen to start with a timestamp.
var postgresLevels = map[string]bool{
	"DEBUG": true, "DEBUG1": true, "DEBUG2": true, "DEBUG3": true,
	"DEBUG4": true, "DEBUG5": true,
	"INFO": true, "NOTICE": true, "WARNING": true, "ERROR": true,
	"LOG": true, "FATAL": true, "PANIC": true,
	"STATEMENT": true, "DETAIL": true, "HINT": true, "CONTEXT": true,
	"QUERY": true, "LOCATION": true,
}

func (p *postgresParser) Detect(sample [][]byte) float64 {
	if len(sample) == 0 {
		return 0
	}

	var considered, matched int
	for _, line := range sample {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		// Continuation lines are indented and carry no timestamp; counting
		// them as failures would understate confidence on a file full of
		// multi-line statements.
		if p.IsContinuation(line) {
			continue
		}
		considered++

		m := postgresRe.FindSubmatch(line)
		if m != nil && postgresLevels[string(m[5])] {
			matched++
		}
	}
	if considered == 0 {
		return 0
	}
	return float64(matched) / float64(considered)
}

// IsContinuation reports whether a line continues the record above it.
//
// Postgres indents the continuation of a multi-line statement with a tab.
func (p *postgresParser) IsContinuation(line []byte) bool {
	return len(line) > 0 && (line[0] == '\t' || bytes.HasPrefix(line, []byte("    ")))
}

func (p *postgresParser) Parse(line []byte) (Record, error) {
	m := postgresRe.FindSubmatch(line)
	if m == nil {
		return Record{}, ErrNoMatch
	}

	severity := string(m[5])
	if !postgresLevels[severity] {
		return Record{}, ErrNoMatch
	}

	rec := Record{
		Level:   NormaliseLevel(severity),
		Message: string(m[6]),
		Fields:  make(map[string]any, 4),
	}

	// Keep the original word: LOG, STATEMENT, and DETAIL all normalise to info
	// but mean different things, and throwing that away loses information the
	// reader may want.
	//
	// Unconditionally. It used to be stored only when it differed from the
	// normalised level, which is a compression that saves nothing and costs the
	// four severities that matter most: ERROR, FATAL, HINT and CONTEXT all
	// lowercase to their own normalised form, so pg_severity was null for every
	// one of them while `loupe fields` went on advertising the field. A filter
	// written against a documented, autocompleted column returned zero on a file
	// containing 1,825 matching lines.
	rec.Fields["pg_severity"] = severity

	zone := string(m[2])
	rec.Timestamp, rec.TimestampZoned = postgresTime(string(m[1]), zone)
	if !rec.TimestampZoned && zone != "" && zone[0] != '+' && zone[0] != '-' {
		// The line said which zone it meant; loupe could not turn that into an
		// offset. Saying so is the difference between the reader knowing to
		// pass --source-tz and the reader trusting a timeline that is out by
		// the whole offset.
		rec.ZoneAbbrev = zone
	}

	if pid, err := strconv.ParseInt(string(m[3]), 10, 64); err == nil {
		rec.Fields["pid"] = pid
	}

	if userDB := string(m[4]); userDB != "" {
		if i := strings.LastIndex(userDB, "@"); i > 0 {
			rec.Fields["user"] = userDB[:i]
			rec.Fields["db"] = userDB[i+1:]
		}
	}

	return rec, nil
}

// postgresTime combines the timestamp with whatever zone marker followed it.
//
// A numeric offset places the instant exactly. A bare "UTC" does too, since it
// is unambiguous. Any other abbreviation does not: BST, CST, and IST each name
// more than one zone, and guessing would silently move records by hours. Those
// are reported as zoneless so the source's assumed zone applies and the
// assumption is disclosed.
func postgresTime(stamp, zone string) (time.Time, bool) {
	switch {
	case zone == "":
		t, zoned, ok := ParseTime(stamp, time.UTC)
		if !ok {
			return time.Time{}, false
		}
		return t, zoned

	case zone == "UTC" || zone == "GMT" || zone == "Z":
		t, _, ok := ParseTime(stamp, time.UTC)
		if !ok {
			return time.Time{}, false
		}
		return t, true

	case zone[0] == '+' || zone[0] == '-':
		if t, zoned, ok := ParseTime(stamp+" "+zone, time.UTC); ok {
			return t, zoned
		}
		t, _, ok := ParseTime(stamp, time.UTC)
		if !ok {
			return time.Time{}, false
		}
		return t, false

	default:
		// An ambiguous abbreviation. Parse the wall clock and let the source's
		// assumed zone decide, rather than pretending to know which CST.
		t, _, ok := ParseTime(stamp, time.UTC)
		if !ok {
			return time.Time{}, false
		}
		return t, false
	}
}
