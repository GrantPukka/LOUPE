package parse

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
)

func init() { Register(&consoleParser{}) }

// consoleParser reads elapsed-prefixed console output — the shape a CI runner
// writes, and the shape of anything else built to be watched in a terminal
// rather than read from disk later.
//
//	[00:00:31] \x1b[31m✖\x1b[0m FAIL  src/payments/__tests__/authorize.spec.ts
//	[00:00:11] • npm WARN deprecated glob@7.2.3
//
// The prefix is time since the job started, not a wall clock, so a record from
// this format has no timestamp and never will. That is a fact about the format,
// not a failure to read it: `elapsed_s` is the real value, `ts:none` finds these
// records, and any time filter reports them as excluded rather than pretending
// they were somewhere on the timeline. Inventing an absolute time by adding the
// elapsed seconds to some guessed job start would be exactly the silent
// assumption docs/FILTER-DSL.md section 2.5 exists to prevent.
//
// Recognising them at all is the point: 1,718 lines of a build log reported as
// damage look like an ingest problem, and reported as a format with no clock
// look like what they are.
type consoleParser struct{}

func (p *consoleParser) Name() string { return "console" }

// consoleRe matches the bracketed elapsed prefix, optionally with hours, and
// whatever the runner printed after it.
var consoleRe = regexp.MustCompile(
	`^\[((?:(\d{1,2}):)?(\d{2}):(\d{2}))\]\s+(.*)$`)

// consoleMarkerRe matches a leading status glyph, which runners use in place of
// a severity word. It is stripped from the message and kept as a field, because
// the glyph is the only thing distinguishing a passing step from a failing one
// in some runners' output.
var consoleMarkerRe = regexp.MustCompile(`^([✔✓✅✖✗❌⚠•»·×]+)\s+`)

// consoleOutcomes are the words CI runners use where another format writes a
// level. They are checked before the generic severity words so that a line
// reading `FAIL  src/…` is an error even though it never says "error".
var consoleOutcomes = map[string]string{
	"FAIL": LevelError, "FAILED": LevelError, "ERR": LevelError,
	"PASS": LevelInfo, "PASSED": LevelInfo, "OK": LevelInfo, "DONE": LevelInfo,
	"SKIP": LevelWarn, "SKIPPED": LevelWarn,
}

func (p *consoleParser) Detect(sample [][]byte) float64 {
	if len(sample) == 0 {
		return 0
	}

	var considered, matched int
	for _, line := range sample {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		considered++
		if consoleRe.Match(line) {
			matched++
		}
	}
	if considered == 0 {
		return 0
	}
	return float64(matched) / float64(considered)
}

func (p *consoleParser) Parse(line []byte) (Record, error) {
	m := consoleRe.FindSubmatch(line)
	if m == nil {
		return Record{}, ErrNoMatch
	}

	rec := Record{Fields: make(map[string]any, 4)}
	rec.Fields["elapsed"] = string(m[1])
	rec.Fields["elapsed_s"] = consoleElapsed(m[2], m[3], m[4])

	// The colours are for a terminal nobody is watching any more. They are
	// stripped from the message and left untouched in raw.
	message := StripANSI(string(m[5]))
	if marker := consoleMarkerRe.FindStringSubmatch(message); marker != nil {
		rec.Fields["marker"] = marker[1]
		message = message[len(marker[0]):]
	}
	rec.Message = strings.TrimSpace(message)

	rec.Level = consoleLevel(rec.Message)

	return rec, nil
}

// consoleElapsed converts the prefix to seconds so elapsed_s:>300 works.
func consoleElapsed(hours, minutes, seconds []byte) int64 {
	n := func(b []byte) int64 {
		v, _ := strconv.ParseInt(string(b), 10, 64)
		return v
	}
	return n(hours)*3600 + n(minutes)*60 + n(seconds)
}

// consoleLevel reads the outcome word a runner puts at the head of a line,
// falling back to the ordinary severity words in the text.
func consoleLevel(message string) string {
	word, _, _ := strings.Cut(message, " ")
	if level, ok := consoleOutcomes[strings.ToUpper(word)]; ok {
		return level
	}
	return levelFromMessage(message)
}
