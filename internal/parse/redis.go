package parse

import (
	"bytes"
	"regexp"
	"strconv"
	"time"
)

func init() { Register(&redisParser{}) }

// redisParser reads the Redis server log.
//
//	8:M 20 Aug 2026 21:00:06.859 * Background saving started by pid 7480
//	21:C 20 Aug 2026 21:00:15.208 # Redis is now ready to exit
//
// The leading `pid:role` is Redis's own, where the role letter says which
// process wrote the line — M for the master, S for a replica, C for a forked
// child doing a save, X for a sentinel. Keeping it is the difference between
// "the database logged this" and "the background save child logged this",
// which is exactly the distinction you need when a save is what went wrong.
//
// The severity is a single punctuation character, and the timestamp carries no
// offset, so records are reported as zoneless.
type redisParser struct{}

func (p *redisParser) Name() string { return "redis" }

var redisRe = regexp.MustCompile(
	`^(\d+):([MCSX]) (\d{1,2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2}\.\d{3}) ([.\-*#]) (.*)$`)

// redisLayout is Redis's own timestamp format. It is not in Layouts because
// nothing else writes it and ParseTime would try it against every other
// format's timestamps for nothing.
const redisLayout = "2 Jan 2006 15:04:05.000"

// redisRoles names the process that wrote the line.
var redisRoles = map[string]string{
	"M": "master", "C": "child", "S": "replica", "X": "sentinel",
}

// redisLevels maps Redis's punctuation severities onto the canonical set.
// Redis calls them debug, verbose, notice and warning; warning is the one it
// uses for anything actually wrong.
var redisLevels = map[string]string{
	".": LevelDebug, "-": LevelTrace, "*": LevelInfo, "#": LevelWarn,
}

func (p *redisParser) Detect(sample [][]byte) float64 {
	if len(sample) == 0 {
		return 0
	}

	var considered, matched int
	for _, line := range sample {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		considered++
		if redisRe.Match(line) {
			matched++
		}
	}
	if considered == 0 {
		return 0
	}
	return float64(matched) / float64(considered)
}

func (p *redisParser) Parse(line []byte) (Record, error) {
	m := redisRe.FindSubmatch(line)
	if m == nil {
		return Record{}, ErrNoMatch
	}

	rec := Record{
		Level:   redisLevels[string(m[4])],
		Message: string(m[5]),
		Fields:  make(map[string]any, 4),
	}

	if pid, err := strconv.ParseInt(string(m[1]), 10, 64); err == nil {
		rec.Fields["pid"] = pid
	}
	if role, ok := redisRoles[string(m[2])]; ok {
		rec.Fields["role"] = role
	}

	// Parsed here rather than through ParseTime: the layout is Redis-only, and
	// the record is zoneless because the format writes no offset.
	if ts, err := time.ParseInLocation(redisLayout, string(m[3]), time.UTC); err == nil {
		rec.Timestamp, rec.TimestampZoned = ts, false
	}

	return rec, nil
}
