package parse

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// jsonMember is one key and value from a JSON object, in the order the line
// wrote them.
//
// A slice rather than a map because both facts matter: order decides which of
// two `ts` keys wins, and keeping repeats is what stops the fields bag dropping
// one — encoding/json into a map[string]any resolves a repeated member by
// last-write-wins, silently.
type jsonMember struct {
	key   string
	value any
}

// decodeJSONObject reads a JSON object member by member, salvaging what is
// there when the object ends early.
//
// A truncated final line is the commonest corruption a log file has: a
// rotation, a full disk, a killed process. The whole object failing to decode
// meant a real cart-svc ERROR with four complete, well-formed keys in front of
// the cut appeared in no filter, no `top service`, no histogram and no
// timeline — invisible, on a tool whose first principle is that nothing goes
// missing quietly. Reading the members that did arrive and marking the record
// turns that into visible-with-a-caveat.
//
// It returns nil only when there is nothing at all to salvage: not an object,
// or cut before the first complete member.
func decodeJSONObject(line []byte) (members []jsonMember, truncated bool) {
	if members, ok := decodeWholeObject(line); ok {
		return members, false
	}
	return decodeObjectMembers(line)
}

// decodeWholeObject is the fast path: one bulk decode into a map, taken only
// when the line is complete and has no repeated member.
//
// It exists because the member-by-member path is worth about twice the CPU and
// three times the allocations — Decoder.Token boxes and allocates every key and
// every string — and on a well-formed line with unique keys it buys nothing at
// all. The count of top-level members is read off the bytes without decoding
// anything, so the check costs a single pass and no allocation; disagreeing
// with the map's size is exactly the condition under which a member was
// silently overwritten.
//
// Map iteration order is deliberately not relied on: nothing here depends on
// which member comes first, because a line that could disagree about that took
// the ordered path instead.
func decodeWholeObject(line []byte) ([]jsonMember, bool) {
	n, ok := countObjectMembers(line)
	if !ok {
		return nil, false
	}

	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()

	var raw map[string]any
	if err := dec.Decode(&raw); err != nil || len(raw) != n {
		return nil, false
	}

	members := make([]jsonMember, 0, n)
	for key, value := range raw {
		members = append(members, jsonMember{key: key, value: value})
	}
	return members, true
}

// countObjectMembers counts the top-level members of a JSON object by scanning
// the bytes, and reports whether the object is closed.
//
// One member is one colon at depth one. Colons nested deeper belong to a
// sub-document and colons inside a string are text, so both are skipped, which
// is all the structure this needs to know.
func countObjectMembers(line []byte) (int, bool) {
	if len(line) == 0 || line[0] != '{' {
		return 0, false
	}

	depth, members := 0, 0
	inString, escaped := false, false

	for _, c := range line {
		switch {
		case escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// Punctuation inside a string is text.
		case c == '{' || c == '[':
			depth++
		case c == '}' || c == ']':
			depth--
		case c == ':' && depth == 1:
			members++
		}
	}

	// depth back to zero means the object closed; an unterminated string or a
	// missing brace both leave it positive, and both mean the line was cut.
	return members, depth == 0 && !inString
}

// decodeObjectMembers reads the object one member at a time, keeping the order
// the line wrote them and every repeat of a key.
func decodeObjectMembers(line []byte) (members []jsonMember, truncated bool) {
	// json.Number keeps integer IDs from being mangled into float64 and
	// re-rendered in scientific notation, which silently corrupts values like a
	// 19-digit trace id.
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()

	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}

	for {
		keyTok, err := dec.Token()
		if err != nil {
			// Out of input in the middle of the object.
			return members, err != io.EOF || len(members) > 0
		}
		if keyTok == json.Delim('}') {
			return members, false
		}

		key, ok := keyTok.(string)
		if !ok {
			// A non-string member name is not JSON. Whatever this is, it is not
			// an object being read correctly.
			return members, true
		}

		valueTok, err := dec.Token()
		if err != nil {
			// The key arrived and its value did not. The key alone says
			// nothing, so it is dropped and the record marked instead.
			return members, true
		}
		value, err := decodeJSONValue(dec, valueTok, 0)
		if err != nil {
			return members, true
		}
		members = append(members, jsonMember{key: key, value: value})
	}
}

// maxJSONDepth bounds how deep decodeJSONValue will recurse.
//
// A log line nested two hundred objects deep is not a log line, and a decoder
// that follows one as far as it goes hands a hostile file a stack overflow.
const maxJSONDepth = 200

// errJSONTooDeep is returned past maxJSONDepth. Like every other decode
// failure here it costs the record its remaining fields, not the file.
var errJSONTooDeep = errors.New("json object nested too deeply")

// decodeJSONValue builds one value from the token stream, given the token that
// opens it.
//
// The tokeniser does all the lexing, so this is not a second JSON parser — it
// is the assembly step that Decoder.Decode would otherwise do. Doing it here is
// worth the thirty lines: Decode restarts its own scan of the remaining input
// for every member, and on a 1.5M-record file that was a third of the ingest.
func decodeJSONValue(dec *json.Decoder, tok json.Token, depth int) (any, error) {
	delim, nested := tok.(json.Delim)
	if !nested {
		// A string, a json.Number, a bool, or nil — already the value.
		return tok, nil
	}
	if depth >= maxJSONDepth {
		return nil, errJSONTooDeep
	}

	switch delim {
	case '{':
		obj := map[string]any{}
		for {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			if keyTok == json.Delim('}') {
				return obj, nil
			}
			key, ok := keyTok.(string)
			if !ok {
				return nil, errNotAnObject
			}
			valueTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			value, err := decodeJSONValue(dec, valueTok, depth+1)
			if err != nil {
				return nil, err
			}
			// A repeated key inside a nested object still collapses. The bag
			// is the surface people filter on and the surface the rule is
			// about; suffixing inside an arbitrary sub-document would change
			// the shape of data the writer chose.
			obj[key] = value
		}

	case '[':
		arr := []any{}
		for {
			valueTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			if valueTok == json.Delim(']') {
				return arr, nil
			}
			value, err := decodeJSONValue(dec, valueTok, depth+1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, value)
		}
	}

	return nil, errNotAnObject
}

// errNotAnObject reports a closing delimiter where a value was expected, which
// only a malformed document produces.
var errNotAnObject = errors.New("unexpected json delimiter")
