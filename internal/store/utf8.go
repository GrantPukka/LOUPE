package store

import (
	"encoding/hex"
	"strings"
	"unicode/utf8"

	"github.com/GrantPukka/loupe/internal/parse"
)

// RawHexField is the fields-bag key holding the original bytes of a record
// whose text was not valid UTF-8.
//
// The name is prefixed so that it cannot collide with a key a log actually
// carries, and it is a field rather than a column so that recovering the bytes
// needs no schema change and costs nothing on the 99.9996% of records that are
// clean.
const RawHexField = "loupe_raw_hex"

// RawHexExpr reads RawHexField in SQL.
//
// The field is kept in the JSON bag rather than promoted to a column — see
// collectSamples, which deletes it before schema inference so a hex dump never
// lands next to the fields someone actually filters on. That decision is right
// and it means `SELECT loupe_raw_hex FROM logs` does not resolve, so anything
// telling a user where the bytes are has to hand them this instead of a bare
// column name.
const RawHexExpr = `fields->>'$."` + RawHexField + `"'`

// sanitiseEntry replaces invalid UTF-8 in a record's text with U+FFFD, keeping
// the original bytes hex-encoded in the fields bag.
//
// DuckDB rejects invalid UTF-8 at the appender, and rejects it by invalidating
// the whole batch: one bad byte in one line of a 250,000-line file aborts the
// entire ingest with an error naming neither the file nor the line. A log
// explorer whose whole purpose is reading logs that are already a mess cannot
// fall over on binary garbage in a log line.
//
// Replacing rather than dropping is what keeps the record — and its timestamp,
// its level, and its position in the file — on the timeline. The hex copy is
// what keeps the promise that nothing is lost: the bytes are still there, they
// are just no longer pretending to be text.
//
// It reports whether anything was replaced, so the count can be stated. Note
// that `lower()` in DuckDB does not merely reject invalid UTF-8, it hangs on
// it, so this also decides whether case-insensitive search works at all.
func sanitiseEntry(e *parse.Entry) bool {
	if utf8.ValidString(e.Raw) &&
		utf8.ValidString(e.Message) &&
		utf8.ValidString(e.Level) &&
		fieldsValid(e.Fields) {
		return false
	}

	original := e.Raw

	e.Raw = strings.ToValidUTF8(e.Raw, "�")
	e.Message = strings.ToValidUTF8(e.Message, "�")
	e.Level = strings.ToValidUTF8(e.Level, "�")
	e.Fields = sanitiseFields(e.Fields)

	if e.Fields == nil {
		e.Fields = map[string]any{}
	}
	// Never overwrite a key the log itself carried: the bag is the one place
	// where an unrecognised field is guaranteed to survive.
	if _, taken := e.Fields[RawHexField]; !taken {
		e.Fields[RawHexField] = hex.EncodeToString([]byte(original))
	}

	return true
}

// fieldsValid reports whether every string in the bag is valid UTF-8.
//
// Keys are checked too. A key is a column name as far as promotion and the
// filter DSL are concerned, and an unnamable column is worse than a renamed
// one.
func fieldsValid(fields map[string]any) bool {
	for k, v := range fields {
		if !utf8.ValidString(k) {
			return false
		}
		if s, ok := v.(string); ok && !utf8.ValidString(s) {
			return false
		}
	}
	return true
}

// sanitiseFields rewrites invalid UTF-8 in the bag's keys and string values.
//
// Only strings are touched. Numbers and booleans came from a decoder that
// already rejected malformed input, and nested values arrive as decoded Go
// types that json.Marshal will re-encode safely.
func sanitiseFields(fields map[string]any) map[string]any {
	if len(fields) == 0 {
		return fields
	}

	out := make(map[string]any, len(fields))
	for k, v := range fields {
		if s, ok := v.(string); ok {
			v = strings.ToValidUTF8(s, "�")
		}
		out[strings.ToValidUTF8(k, "�")] = v
	}
	return out
}
