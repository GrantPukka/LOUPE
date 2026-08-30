package query

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Schema is what the compiler needs to know about the loaded data: which
// columns exist, which JSON field keys are present, and which logical sources
// were ingested.
//
// It is passed in rather than queried here so that the query package never
// touches the database, which keeps compilation testable without one.
type Schema struct {
	// Fields are the distinct keys present in the fields bag.
	Fields []string
	// Sources are the distinct logical source names, for prefix matching.
	Sources []string
	// Promoted maps a field name to the real column it was given during
	// schema inference. A promoted field compiles to a bare column reference
	// rather than a JSON extraction evaluated per row.
	Promoted map[string]string
}

// columns are the promoted columns, mapped to how they are referenced in SQL.
//
// These are fixed in v1. Dynamic promotion arrives with internal/schema and
// will add to this set rather than change how it is consulted.
var columns = map[string]string{
	"ts":       "ts",
	"level":    "level",
	"message":  "message",
	"msg":      "message",
	"source":   "source",
	"file":     "file",
	"format":   "format",
	"line_no":  "line_no",
	"line":     "line_no",
	"raw":      "raw",
	"parsed":   "parsed",
	"seq":      "seq",
	"ts_zoned": "ts_zoned",

	// pattern names a message template by its id, not by its text. The id is
	// what `loupe patterns` prints beside each template and the only handle
	// short enough to type, so the DSL term takes the handle. The template
	// text lives in the pattern column and is not addressable on its own.
	"pattern": "pattern_id",
}

// Known reports every field name a user could reference, for error messages.
func (s Schema) Known() []string {
	seen := map[string]bool{}
	var out []string

	for name := range columns {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for name := range s.Promoted {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, f := range s.Fields {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}

	sort.Strings(out)
	return out
}

// Column returns the SQL expression that reads a field, whether it is a real
// column, a promoted one, or still a key in the JSON bag.
//
// Exported for the callers that need to group by a field rather than filter on
// one — `loupe top` most of all. Sharing the resolver means a facet and a
// filter cannot disagree about which column a name refers to, and an unknown
// name produces the same error with the same spelling suggestion in both.
func (s Schema) Column(key string) (string, error) { return s.resolve(key) }

// resolve maps a key to the SQL expression that reads it.
//
// A key that is neither a column nor a present JSON field is an error, never a
// silently empty result. That rule exists because a typo'd field name returning
// zero rows is the most common way this kind of tool tells someone a confident
// lie.
func (s Schema) resolve(key string) (expr string, err error) {
	lower := strings.ToLower(key)

	if col, ok := columns[lower]; ok {
		return col, nil
	}

	// A promoted field is a real typed column, so read it directly instead of
	// extracting it from JSON on every row.
	if col, ok := s.Promoted[key]; ok {
		return quoteIdent(col), nil
	}
	for name, col := range s.Promoted {
		if strings.EqualFold(name, key) {
			return quoteIdent(col), nil
		}
	}

	for _, f := range s.Fields {
		if f == key {
			return jsonPath(f), nil
		}
	}
	// Case-insensitive second pass, so Status finds status.
	for _, f := range s.Fields {
		if strings.EqualFold(f, key) {
			return jsonPath(f), nil
		}
	}

	return "", s.unknownField(key)
}

// jsonPath builds the extraction expression for a field in the JSON bag.
//
// The key is embedded in a JSON path literal rather than passed as a parameter
// because DuckDB does not accept a placeholder there. That puts it inside two
// nested quoting contexts at once — a $."..." path, itself inside a '...' SQL
// string literal — and both have to be escaped. A field named a'b closes the
// SQL literal early and the remainder of the path is parsed as SQL.
//
// resolve only reaches this with a key already present in the data, so the
// value comes from the database's own contents rather than the query string.
// It is escaped anyway: a log file is not a trusted input.
//
// The result is parenthesised because ->> binds looser than both :: and AND in
// DuckDB. Unbracketed, `format = ? AND fields->>'$."k"' = ?` parses as
// `format = ? AND fields ->> ('$."k"' = ?)` and fails at run time with a type
// error naming a value from some unrelated record — so every bag field became
// unqueryable the moment it was combined with a second term, which is most of
// the time. The parentheses make the extraction one expression whatever it is
// dropped into.
func jsonPath(key string) string {
	return `(fields->>'$."` + escapeJSONPathKey(key) + `"')`
}

// BagPath reports whether expr is a fields-bag extraction, and returns the JSON
// path literal inside it.
//
// Callers ask this rather than matching the prefix themselves. The expression's
// exact spelling is jsonPath's business — it grew a pair of parentheses to fix
// an operator-precedence bug, and a caller string-matching "fields->>" silently
// reclassified every bag field as a real column the moment it did.
func BagPath(expr string) (string, bool) {
	const open, prefix, close = "(", "fields->>", ")"
	if !strings.HasPrefix(expr, open+prefix) || !strings.HasSuffix(expr, close) {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(expr, open+prefix), close), true
}

// escapeJSONPathKey escapes a field name for both quoting contexts it lands in.
//
// Backslash goes first, or it escapes the backslashes the next step adds.
func escapeJSONPathKey(key string) string {
	esc := strings.ReplaceAll(key, `\`, `\\`)
	esc = strings.ReplaceAll(esc, `"`, `\"`)
	return strings.ReplaceAll(esc, `'`, `''`)
}

// quoteIdent wraps a column name in double quotes, doubling any inside.
//
// Promoted column names are generated by the schema package from keys already
// in the data, but quoting keeps a field named "order" or "select" from
// colliding with SQL keywords.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// UnknownFieldError names a field that does not exist and suggests what the
// user probably meant.
type UnknownFieldError struct {
	Key         string
	Suggestions []string
	Available   []string
}

func (e *UnknownFieldError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "unknown field %q", e.Key)

	if len(e.Suggestions) > 0 {
		fmt.Fprintf(&sb, "\ndid you mean: %s", strings.Join(displayNames(e.Suggestions), ", "))
	}

	if len(e.Available) > 0 {
		const show = 20
		list := e.Available
		suffix := ""
		if len(list) > show {
			list, suffix = list[:show], fmt.Sprintf(", … and %d more", len(e.Available)-show)
		}
		fmt.Fprintf(&sb, "\nfields present in this data: %s%s",
			strings.Join(displayNames(list), ", "), suffix)
	}

	return sb.String()
}

// displayNames makes field names safe to print in a terminal.
//
// A key can contain anything the log file contained. Writing raw control bytes
// to a terminal makes a real field look like a rendering fault — a NUL prints
// as nothing, so iss\0\0uer appears as a word with a hole in it, and the user
// reasonably concludes the tool is broken rather than the log.
//
// Only awkward names are quoted, so the common case stays a plain readable
// list, and quoting marks out the name that needs explaining.
func displayNames(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		if strings.ContainsFunc(n, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			out[i] = strconv.Quote(n)
			continue
		}
		out[i] = n
	}
	return out
}

func (s Schema) unknownField(key string) error {
	available := s.Known()
	return &UnknownFieldError{
		Key:         key,
		Suggestions: suggest(key, available),
		Available:   available,
	}
}

// suggest returns the closest known names to key.
//
// Distance is capped relative to the length of what was typed, so a short typo
// gets a tight threshold and a long one gets more slack. Returning three bad
// guesses is worse than returning none: the point is to save a user from
// re-reading the docs, not to fill the error with noise.
func suggest(key string, available []string) []string {
	lower := strings.ToLower(key)

	maxDist := 1
	switch {
	case len(lower) > 10:
		maxDist = 3
	case len(lower) > 5:
		maxDist = 2
	}

	type scored struct {
		name string
		dist int
	}
	var candidates []scored

	for _, name := range available {
		other := strings.ToLower(name)

		// A prefix or containment match is almost always what was meant, and
		// edit distance scores those poorly when the lengths differ.
		if strings.HasPrefix(other, lower) || strings.HasPrefix(lower, other) ||
			strings.Contains(other, lower) {
			candidates = append(candidates, scored{name, 0})
			continue
		}
		if d := editDistance(lower, other); d <= maxDist {
			candidates = append(candidates, scored{name, d})
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].dist != candidates[j].dist {
			return candidates[i].dist < candidates[j].dist
		}
		return candidates[i].name < candidates[j].name
	})

	const maxSuggestions = 4
	out := make([]string, 0, maxSuggestions)
	for _, c := range candidates {
		if len(out) == maxSuggestions {
			break
		}
		out = append(out, c.name)
	}
	return out
}

// editDistance is Levenshtein distance, iterative with two rows.
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len([]rune(b))
	}
	if len(b) == 0 {
		return len([]rune(a))
	}

	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)

	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}

	return prev[len(rb)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

// AmbiguousSourceError reports a source prefix matching more than one source.
type AmbiguousSourceError struct {
	Prefix     string
	Candidates []string
}

func (e *AmbiguousSourceError) Error() string {
	return fmt.Sprintf("source %q is ambiguous: matches %s",
		e.Prefix, strings.Join(e.Candidates, ", "))
}

// resolveSource expands a source prefix to a full name.
//
// Prefix matching is a convenience worth having — source:check finding
// checkout-api saves real typing during an incident — but an ambiguous prefix
// must error and list the candidates rather than silently picking one.
//
// An unrecognised name is returned unchanged rather than rejected: the source
// may legitimately not be in this data, and an empty result for source:foo is
// a meaningful answer in a way that an empty result for a typo'd field is not.
func (s Schema) resolveSource(value string) (string, error) {
	for _, name := range s.Sources {
		if name == value {
			return name, nil
		}
	}

	var matches []string
	for _, name := range s.Sources {
		if strings.HasPrefix(strings.ToLower(name), strings.ToLower(value)) {
			matches = append(matches, name)
		}
	}

	switch len(matches) {
	case 0:
		return value, nil
	case 1:
		return matches[0], nil
	default:
		sort.Strings(matches)
		return "", &AmbiguousSourceError{Prefix: value, Candidates: matches}
	}
}
