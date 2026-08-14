package parse

import (
	"strconv"
	"strings"
	"time"
)

// Layouts are the timestamp formats tried when a field's layout is not known
// in advance, most specific first.
//
// Adding a layout here is a genuinely useful two-line contribution. Order
// matters: time.Parse accepts the first layout that fits, so a more specific
// layout must come before a prefix of itself.
//
// This table lives in parse rather than schema because parsers need it
// directly, and parse may not import anything downstream of it.
var Layouts = []string{
	time.RFC3339Nano,                       // 2026-08-13T14:02:00.021472370Z
	time.RFC3339,                           // 2026-08-13T14:02:00Z
	"2006-01-02T15:04:05.999999999",        // no zone: caller applies the assumed one
	"2006-01-02T15:04:05",                  //
	"2006-01-02 15:04:05.999999 MST",       // postgres
	"2006-01-02 15:04:05.999999-07",        // postgres with a numeric offset
	"2006-01-02 15:04:05.999999999 -07:00", // colon-separated offset
	"2006-01-02 15:04:05.999999999 -0700",
	"2006-01-02 15:04:05.999999999 -07",
	"2006-01-02 15:04:05.999999999",    // log4j default
	"2006-01-02 15:04:05",              //
	"02/Jan/2006:15:04:05 -0700",       // nginx and apache combined
	"02/Jan/2006:15:04:05",             //
	"Jan _2 15:04:05",                  // syslog RFC3164, no year
	"Jan 02 15:04:05",                  //
	"2006/01/02 15:04:05",              // nginx error log
	time.RFC1123Z,                      // Mon, 02 Jan 2006 15:04:05 -0700
	time.RFC1123,                       //
	"2006-01-02T15:04:05.999999999Z07", // some java writers emit a bare hour offset
	"20060102T150405Z",                 // compact ISO
	"20060102 150405",                  //
}

// zonelessLayouts are the layouts in Layouts that carry no timezone
// information. A timestamp parsed with one of these is in the source's assumed
// timezone, which docs/FILTER-DSL.md section 2.5 requires be disclosed rather
// than guessed at silently.
var zonelessLayouts = map[string]bool{
	"2006-01-02T15:04:05.999999999": true,
	"2006-01-02T15:04:05":           true,
	"2006-01-02 15:04:05.999999999": true,
	"2006-01-02 15:04:05":           true,
	"02/Jan/2006:15:04:05":          true,
	"Jan _2 15:04:05":               true,
	"Jan 02 15:04:05":               true,
	"2006/01/02 15:04:05":           true,
	"20060102 150405":               true,
}

// ParseTime tries every known layout, then epoch numbers.
//
// loc is the timezone applied to layouts that carry none. Callers pass the
// source's assumed zone, which defaults to UTC because servers overwhelmingly
// run UTC and the wrong default here is worse than a surprising one.
//
// The returned zoned reports whether the timestamp carried its own zone. A
// false value means the result depends on an assumption the user must be told
// about.
func ParseTime(s string, loc *time.Location) (t time.Time, zoned bool, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false, false
	}
	if loc == nil {
		loc = time.UTC
	}

	for _, layout := range Layouts {
		if zonelessLayouts[layout] {
			parsed, err := time.ParseInLocation(layout, s, loc)
			if err != nil {
				continue
			}
			return normaliseYear(parsed, layout), false, true
		}
		parsed, err := time.Parse(layout, s)
		if err != nil {
			continue
		}
		// A layout with a zone abbreviation but no offset, such as postgres's
		// "UTC", yields a zero offset for any abbreviation Go cannot resolve.
		// Treat a resolved zero-offset non-UTC abbreviation as unzoned so the
		// assumption is still disclosed.
		if name, offset := parsed.Zone(); offset == 0 && name != "UTC" && name != "" {
			return parsed, false, true
		}
		return parsed, true, true
	}

	return parseEpoch(s, loc)
}

// parseEpoch handles numeric timestamps, distinguishing seconds, milliseconds,
// microseconds, and nanoseconds by magnitude.
//
// The thresholds are deliberately narrow. A bare integer is far more often an
// ID, a port, or a status code than a timestamp, so anything outside a
// plausible date range is rejected rather than turned into 1970.
func parseEpoch(s string, loc *time.Location) (time.Time, bool, bool) {
	if strings.ContainsAny(s, "-:/ T") {
		return time.Time{}, false, false
	}

	// A decimal point means seconds with a fraction, which is unambiguous.
	if dot := strings.IndexByte(s, '.'); dot > 0 {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || !plausibleSeconds(int64(f)) {
			return time.Time{}, false, false
		}
		sec := int64(f)
		nsec := int64((f - float64(sec)) * 1e9)
		return time.Unix(sec, nsec).UTC(), true, true
	}

	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, false, false
	}

	switch {
	case plausibleSeconds(n):
		return time.Unix(n, 0).UTC(), true, true
	case plausibleSeconds(n / 1e3):
		return time.UnixMilli(n).UTC(), true, true
	case plausibleSeconds(n / 1e6):
		return time.UnixMicro(n).UTC(), true, true
	case plausibleSeconds(n / 1e9):
		return time.Unix(0, n).UTC(), true, true
	}
	return time.Time{}, false, false
}

// plausibleSeconds bounds epoch seconds to 2001-09-09 through 2033-05-18. Log
// timestamps outside that window are far likelier to be some other number.
func plausibleSeconds(sec int64) bool {
	return sec >= 1_000_000_000 && sec < 2_000_000_000
}

// normaliseYear fills in the year for layouts that omit it, such as RFC3164
// syslog. Go defaults those to year 0, which sorts before everything and makes
// the record invisible in any time window.
//
// The current year is the least-wrong guess available without reading the whole
// file. Callers that know the file's date range should override it.
func normaliseYear(t time.Time, layout string) time.Time {
	if t.Year() != 0 {
		return t
	}
	if !strings.Contains(layout, "2006") {
		now := time.Now().In(t.Location())
		return t.AddDate(now.Year(), 0, 0)
	}
	return t
}
