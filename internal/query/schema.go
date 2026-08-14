package query

import (
	"fmt"
	"sort"
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
	for _, f := range s.Fields {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}

	sort.Strings(out)
	return out
}

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
// because DuckDB does not accept a placeholder there. Everything that could
// come from user input is escaped, and resolve only ever reaches this with a
// key that matched one already present in the data — so the value is drawn from
// the database's own contents, not from the query string.
func jsonPath(key string) string {
	escaped := strings.ReplaceAll(key, `"`, `\"`)
	return `fields->>'$."` + escaped + `"'`
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
		fmt.Fprintf(&sb, "\ndid you mean: %s", strings.Join(e.Suggestions, ", "))
	}

	if len(e.Available) > 0 {
		const show = 20
		list := e.Available
		suffix := ""
		if len(list) > show {
			list, suffix = list[:show], fmt.Sprintf(", … and %d more", len(e.Available)-show)
		}
		fmt.Fprintf(&sb, "\nfields present in this data: %s%s", strings.Join(list, ", "), suffix)
	}

	return sb.String()
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
