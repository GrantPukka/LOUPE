package parse

import "strings"

// The canonical severity levels, in ascending order. level:>=warn depends on
// this ordering, and it is the only ordering the DSL knows about.
const (
	LevelTrace = "trace"
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
	LevelFatal = "fatal"
)

// Levels lists the canonical levels from least to most severe.
var Levels = []string{LevelTrace, LevelDebug, LevelInfo, LevelWarn, LevelError, LevelFatal}

// levelRank maps a canonical level to its ordinal position.
var levelRank = func() map[string]int {
	m := make(map[string]int, len(Levels))
	for i, l := range Levels {
		m[l] = i
	}
	return m
}()

// levelAliases maps the spellings found in the wild onto canonical levels.
//
// Single letters are here because several formats abbreviate, and syslog
// severity words because RFC5424 uses its own vocabulary. Anything not in this
// table keeps its original string rather than being guessed at.
var levelAliases = map[string]string{
	"t": LevelTrace, "trc": LevelTrace, "trace": LevelTrace, "finest": LevelTrace,
	"verbose": LevelTrace, "v": LevelTrace,

	"d": LevelDebug, "dbg": LevelDebug, "debug": LevelDebug, "fine": LevelDebug,

	"i": LevelInfo, "inf": LevelInfo, "info": LevelInfo, "information": LevelInfo,
	"informational": LevelInfo, "notice": LevelInfo, "note": LevelInfo,
	"log": LevelInfo, "statement": LevelInfo, "detail": LevelInfo,

	"w": LevelWarn, "wrn": LevelWarn, "warn": LevelWarn, "warning": LevelWarn,

	"e": LevelError, "err": LevelError, "error": LevelError, "severe": LevelError,
	"eror": LevelError, "exception": LevelError,

	"f": LevelFatal, "ftl": LevelFatal, "fatal": LevelFatal, "crit": LevelFatal,
	"critical": LevelFatal, "alert": LevelFatal, "emerg": LevelFatal,
	"emergency": LevelFatal, "panic": LevelFatal,
}

// NormaliseLevel maps a level string from any format onto the canonical set, so
// that WARNING, warn, and W all compare equal and cross-format filtering works.
//
// An unrecognised level keeps its original string, lowercased. Per
// docs/FILTER-DSL.md section 3 those sort above trace and match only on exact
// equality — inventing a rank for a level nobody defined would be worse than
// admitting it is unranked.
func NormaliseLevel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Trim(s, "[]()<>:.-")
	if s == "" {
		return ""
	}
	if canonical, ok := levelAliases[s]; ok {
		return canonical
	}
	return s
}

// LevelRank returns the ordinal position of a canonical level and whether it is
// ranked at all. Unranked levels sort above trace, so they are never swept up
// by a level:>=warn style comparison.
func LevelRank(level string) (rank int, ranked bool) {
	r, ok := levelRank[level]
	return r, ok
}

// IsLevel reports whether a string is one of the canonical levels.
func IsLevel(s string) bool {
	_, ok := levelRank[s]
	return ok
}
