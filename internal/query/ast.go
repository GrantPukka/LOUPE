package query

import (
	"strings"
)

// Op is a comparison operator on a field term.
type Op string

const (
	// OpEq is equality, the operator when none is written.
	OpEq Op = ""
	OpGT Op = ">"
	OpLT Op = "<"
	OpGE Op = ">="
	OpLE Op = "<="
	// OpMatch is ~, meaning substring or, with a regex value, a pattern match.
	OpMatch Op = "~"
)

// Query is a parsed filter expression.
//
// Terms are joined by AND. There is no OR keyword and no grouping in v1: every
// log tool that adds boolean grouping ends up with a query language nobody can
// remember, and `loupe sql` is the escape hatch for anything this cannot say.
type Query struct {
	Terms []Term
}

// IsEmpty reports whether the query constrains anything.
func (q Query) IsEmpty() bool { return len(q.Terms) == 0 }

// String renders the query back to DSL text.
//
// Round-tripping matters: parse(render(ast)) must equal ast. The UI's timeline
// drag writes a rendered string into the filter box, so rendering is a user-
// facing feature and not only a debugging aid.
func (q Query) String() string {
	parts := make([]string, 0, len(q.Terms))
	for _, t := range q.Terms {
		parts = append(parts, t.String())
	}
	return strings.Join(parts, " ")
}

// Term is one component of a query.
type Term interface {
	// String renders the term back to DSL text.
	String() string
	// negated reports whether the term is inverted. Unexported to keep Term a
	// closed set: the compiler switches over the concrete types, and a term
	// implemented outside this package could not be compiled anyway.
	negated() bool
}

// Value is one value in a term, carrying how it was written so that rendering
// can reproduce it.
type Value struct {
	Text string
	// Quoted means the value was written in double quotes and must be
	// re-quoted when rendered.
	Quoted bool
	// Regex means the value was written between slashes.
	Regex bool
}

func (v Value) String() string {
	switch {
	case v.Regex:
		return "/" + strings.ReplaceAll(v.Text, "/", `\/`) + "/"
	case v.Quoted || needsQuoting(v.Text):
		return quote(v.Text)
	default:
		return v.Text
	}
}

// needsQuoting reports whether a bare value would not survive a round trip.
func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	return strings.ContainsAny(s, " \t\n\",:")
}

// renderKey writes a field name so that parsing the result yields the same key.
func renderKey(key string) string {
	if keyNeedsQuoting(key) {
		return quote(key)
	}
	return key
}

// keyNeedsQuoting reports whether a bare field name would parse back as
// something other than itself.
//
// Two categories: characters the lexer treats specially, and names that collide
// with the DSL's own vocabulary. A field called last is the second kind — bare,
// last:15m is a time term, so it has to be rendered "last":15m to survive the
// round trip.
func keyNeedsQuoting(key string) bool {
	if needsQuoting(key) || strings.ContainsRune(key, '~') {
		return true
	}
	// A leading minus would read as negation of a different key.
	if strings.HasPrefix(key, "-") {
		return true
	}
	return timeKeywords[strings.ToLower(key)] || isClockHour(key)
}

func quote(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			sb.WriteByte('\\')
			sb.WriteRune(r)
		case '\n':
			sb.WriteString(`\n`)
		case '\t':
			sb.WriteString(`\t`)
		default:
			sb.WriteRune(r)
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// FieldTerm constrains a named field: level, source, status, trace_id, and so
// on. Promoted columns and JSON fields are the same thing here; which storage a
// key lives in is resolved at compile time, not parse time.
//
// One term type covers every named field rather than a separate type per key.
// The differences between them are a handful of cases in the compiler, and a
// type hierarchy would not make those cases go away.
type FieldTerm struct {
	Key    string
	Op     Op
	Values []Value
	Negate bool
}

func (t *FieldTerm) negated() bool { return t.Negate }

func (t *FieldTerm) String() string {
	var sb strings.Builder
	if t.Negate {
		sb.WriteByte('-')
	}
	sb.WriteString(renderKey(t.Key))

	// The match operator is written without a colon — message~timeout — which
	// is the form docs/FILTER-DSL.md section 5 uses throughout. Both forms
	// parse; this is the one rendered back to the user.
	if t.Op == OpMatch {
		sb.WriteString("~")
	} else {
		sb.WriteByte(':')
		sb.WriteString(string(t.Op))
	}

	parts := make([]string, len(t.Values))
	for i, v := range t.Values {
		parts[i] = v.String()
	}
	sb.WriteString(strings.Join(parts, ","))

	return sb.String()
}

// FreeTerm is a bare word or quoted phrase, matched against the message and
// every field value — which is what people expect from a search box.
type FreeTerm struct {
	Value  Value
	Negate bool
}

func (t *FreeTerm) negated() bool { return t.Negate }

func (t *FreeTerm) String() string {
	if t.Negate {
		return "-" + t.Value.String()
	}
	return t.Value.String()
}

// TimeTerm constrains the timestamp.
//
// Parsing a time term does not resolve it. Resolution needs the data's own date
// range and the display timezone, neither of which the parser has, and all time
// terms must be intersected into a single interval before compiling so that
// overlapping terms narrow rather than producing redundant predicates.
type TimeTerm struct {
	// Keyword is after, since, before, until, between, last, on, or empty for
	// a bare range like 14:00-15:00.
	Keyword string
	// Expr is the unresolved time expression, exactly as written.
	Expr   string
	Negate bool
}

func (t *TimeTerm) negated() bool { return t.Negate }

func (t *TimeTerm) String() string {
	var sb strings.Builder
	if t.Negate {
		sb.WriteByte('-')
	}
	if t.Keyword != "" {
		sb.WriteString(t.Keyword)
		sb.WriteByte(':')
	}
	sb.WriteString(t.Expr)
	return sb.String()
}

// ResolvedTimeTerm is every time term in a query, intersected into one window.
//
// ResolveTime produces it and Compile consumes it. Resolving to a single
// interval before compiling is what makes overlapping terms narrow rather than
// stack up redundant comparisons, and what keeps the predicate to a single
// index-friendly ts >= ? AND ts < ?.
type ResolvedTimeTerm struct {
	Interval Interval
	// Exclude holds windows removed by negated time terms.
	Exclude []Interval
}

func (t *ResolvedTimeTerm) negated() bool { return false }

// String renders the resolved window as absolute RFC3339 instants.
//
// Rendering absolutes rather than the original text is deliberate: this is what
// the UI's timeline drag puts in the filter box, and a query that gets pasted
// into a ticket has to mean the same window on the reader's machine as it did
// on the writer's.
func (t *ResolvedTimeTerm) String() string {
	parts := make([]string, 0, 1+len(t.Exclude))
	if s := t.Interval.String(); s != "" {
		parts = append(parts, s)
	}
	for _, ex := range t.Exclude {
		if s := ex.String(); s != "" {
			parts = append(parts, "-"+s)
		}
	}
	return strings.Join(parts, " ")
}

// timeKeywords are the prefixes that introduce a time term.
//
// since is an alias for after and until for before, because people reach for
// both by reflex and rejecting one costs a user more than accepting both costs
// us.
var timeKeywords = map[string]bool{
	"after":   true,
	"since":   true,
	"before":  true,
	"until":   true,
	"between": true,
	"last":    true,
	"on":      true,
}

// FieldTerms returns the query's field terms, for callers that need to inspect
// which keys were referenced.
func (q Query) FieldTerms() []*FieldTerm {
	var out []*FieldTerm
	for _, t := range q.Terms {
		if ft, ok := t.(*FieldTerm); ok {
			out = append(out, ft)
		}
	}
	return out
}

// TimeTerms returns the query's time terms.
func (q Query) TimeTerms() []*TimeTerm {
	var out []*TimeTerm
	for _, t := range q.Terms {
		if tt, ok := t.(*TimeTerm); ok {
			out = append(out, tt)
		}
	}
	return out
}
