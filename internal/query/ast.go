package query

import (
	"strings"
	"unicode"
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

	// Stats is the aggregation clause, nil when the query only filters.
	//
	// It is not in Terms because it is not a predicate: it decides what is
	// reported about the matching records, not which records match. A caller
	// that lists records must refuse a query carrying one rather than ignore
	// it — Session.Plan does exactly that.
	Stats *Stats
}

// IsEmpty reports whether the query constrains anything.
//
// A stats clause does not count: it summarises whatever matched, so a query
// that is nothing but `stats count()` still matches every record.
func (q Query) IsEmpty() bool { return len(q.Terms) == 0 }

// String renders the query back to DSL text.
//
// Round-tripping matters: parse(render(ast)) must equal ast. The UI's timeline
// drag writes a rendered string into the filter box, so rendering is a user-
// facing feature and not only a debugging aid.
func (q Query) String() string {
	parts := make([]string, 0, len(q.Terms)+1)
	for _, t := range q.Terms {
		parts = append(parts, t.String())
	}

	// The clause always renders last, whatever order it was typed in. Terms
	// are order-independent, but a grouping list runs to the end of the clause,
	// so a term written after `by level` would be swallowed by it on the way
	// back in.
	if s := q.Stats.String(); s != "" {
		parts = append(parts, s)
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
//
// The whitespace test asks unicode.IsSpace rather than listing characters,
// because that is what lexWord asks when deciding where a bare word ends. A
// hand-written list drifts: it omitted carriage return, so a term containing
// one rendered unquoted and then no longer parsed. Found by FuzzParse.
func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	if strings.ContainsAny(s, `",:`) {
		return true
	}
	return strings.IndexFunc(s, unicode.IsSpace) >= 0
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

	// Byte by byte, not rune by rune. Ranging over a string decodes UTF-8, and
	// an invalid byte decodes to the replacement character — so a filter
	// searching for a raw byte was silently rewritten into a search for U+FFFD
	// the moment the UI rendered it back into the box. Every character escaped
	// here is ASCII, and a multi-byte sequence never contains a byte below
	// 0x80, so copying bytes preserves valid text exactly and invalid bytes
	// verbatim. Found by FuzzParse.
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			sb.WriteByte('\\')
			sb.WriteByte(c)
		case '\n':
			sb.WriteString(`\n`)
		case '\t':
			sb.WriteString(`\t`)
		default:
			sb.WriteByte(c)
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
		parts[i] = renderFieldValue(v)
	}
	sb.WriteString(strings.Join(parts, ","))

	return sb.String()
}

// renderFieldValue writes a value in the position after a colon, where a
// leading operator character means something.
//
// `A:=>` asks for the literal `>` — the `=` is an explicit equals — but
// rendering it bare gave `A:>`, which reads back as a comparison with no value.
// Only this position is affected: a bare `>x` at the start of a filter is
// ordinary text, and quoting it there would be noise. Found by FuzzParse.
func renderFieldValue(v Value) string {
	if !v.Quoted && !v.Regex && leadsWithOperator(v.Text) {
		return quote(v.Text)
	}
	return v.String()
}

// leadsWithOperator reports a value whose first character the lexer would read
// as an operator when it follows a colon. The tilde counts: `A:~foo` is the
// match operator written with a colon, so a value of `~foo` has to be quoted or
// it renders as a match against `foo`.
func leadsWithOperator(s string) bool {
	if s == "" {
		return false
	}
	switch s[0] {
	case '>', '<', '=', '~':
		return true
	}
	return false
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
		return "-" + renderFreeValue(t.Value)
	}
	return renderFreeValue(t.Value)
}

// renderFreeValue writes a bare word in the position that starts a term, where
// the language has a vocabulary of its own.
//
// `stats` there introduces an aggregation, so a free-text search for the word
// has to be quoted or it reads back as a clause with no aggregates. The parser
// marks such a value Quoted when it reads it, so the two agree.
func renderFreeValue(v Value) string {
	if !v.Quoted && !v.Regex && isReservedFree(v.Text) {
		return quote(v.Text)
	}
	return v.String()
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
	sb.WriteString(renderTimeExpr(t.Expr))
	return sb.String()
}

// renderTimeExpr writes a time expression so it lexes back as one token.
//
// Colons stay bare — they are what a time is made of, and `after:"14:00"` would
// be a strange thing to show someone. Anything that would not lex back as one
// token cannot: `after:"14:00 x"` rendered bare as `after:14:00 x`, and reading
// that back kept only the first half. Found by FuzzParse.
func renderTimeExpr(expr string) string {
	// The lexer splits a bare time on its colons and the parser reassembles it,
	// which it can only do when every piece is a word. An empty piece is what
	// `on:":"` renders to — `on::` — and that no longer parses.
	for _, part := range strings.Split(expr, ":") {
		if !isBareTimeWord(part) {
			return quote(expr)
		}
	}
	return expr
}

// isBareTimeWord reports whether one colon-separated piece of a time expression
// would lex back as a single word.
//
// Times are digits, the letters of a unit or an RFC3339 marker, and the few
// marks that join them. Anything else — a space, a quote, a tilde — either ends
// the word or starts an operator, so a piece containing one has to be quoted.
func isBareTimeWord(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case isDigit(c), isLetter(c):
		case c == '-', c == '+', c == '.', c == '_':
		default:
			return false
		}
	}
	return true
}

func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

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
