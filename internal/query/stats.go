package query

import (
	"strconv"
	"strings"
	"time"
)

// statsKeyword introduces an aggregation clause.
//
// It is reserved at the start of a term, which is the cost of putting
// aggregation in the filter language rather than behind a flag. Searching for
// the literal word is one pair of quotes away and the error says so, which is
// the same bargain the time keywords already make.
const statsKeyword = "stats"

// byKeyword separates the aggregates from the groupings.
//
// Unlike stats it is not reserved anywhere else, because it is only read in
// the one position where a grouping list can begin. A field genuinely called
// `by` still groups: `stats count() by by`.
const byKeyword = "by"

// AggFunc is an aggregate function name.
//
// The set is closed and small on purpose. docs/FILTER-DSL.md section 10 is the
// list; `loupe sql` is the escape hatch for anything else, and every function
// added here is one more thing the language has to explain.
type AggFunc string

const (
	AggCount AggFunc = "count"
	AggSum   AggFunc = "sum"
	AggAvg   AggFunc = "avg"
	AggMin   AggFunc = "min"
	AggMax   AggFunc = "max"
	AggP50   AggFunc = "p50"
	AggP95   AggFunc = "p95"
	AggP99   AggFunc = "p99"
)

// AggFuncs lists every aggregate, in the order help text and errors show them.
var AggFuncs = []AggFunc{AggCount, AggSum, AggAvg, AggMin, AggMax, AggP50, AggP95, AggP99}

// aggFuncs maps a written name to its function. Lookups are lower-cased first,
// so P99(latency_ms) works.
var aggFuncs = func() map[string]AggFunc {
	m := make(map[string]AggFunc, len(AggFuncs))
	for _, f := range AggFuncs {
		m[string(f)] = f
	}
	return m
}()

// quantiles maps the percentile functions to the fraction they ask for.
//
// A percentile is spelled p99 rather than percentile(latency_ms, 99) because
// p99 is what people say out loud and what dashboards label. The fraction is a
// constant of the function name, never user input.
var quantiles = map[AggFunc]float64{
	AggP50: 0.50,
	AggP95: 0.95,
	AggP99: 0.99,
}

// Numeric reports whether the function reads its field as a number.
//
// count is the only one that does not: counting the records that carry a field
// is meaningful whatever the field holds.
func (f AggFunc) Numeric() bool { return f != AggCount }

// Aggregate is one aggregate in a stats clause: count(), p99(latency_ms).
//
// Field is empty for count() over every matching record. count(path) is a
// different question — how many records carry a path — and both are worth
// being able to ask.
type Aggregate struct {
	Func  AggFunc
	Field string
}

// String renders the aggregate back to DSL text. It doubles as the column
// heading, so the table says which question each column answers.
func (a Aggregate) String() string {
	return string(a.Func) + "(" + renderAggField(a.Field) + ")"
}

// GroupKey is one grouping: a field, or a bucket of time.
//
// A bin is a grouping rather than a separate clause because that is what it is
// — `by level, bin(1m)` is a breakdown by severity and by minute, and giving
// time its own syntax would only mean explaining why it is different.
type GroupKey struct {
	// Field is the field to group by, empty when Bin is set.
	Field string
	// Bin is the bucket width for a time grouping.
	Bin time.Duration
}

// IsBin reports whether the key buckets time rather than reading a field.
func (g GroupKey) IsBin() bool { return g.Bin > 0 }

func (g GroupKey) String() string {
	if g.IsBin() {
		return "bin(" + renderBinDuration(g.Bin) + ")"
	}
	return renderGroupKey(g.Field)
}

// Stats is the aggregation clause of a query.
//
// It is not a Term: it does not constrain which records match, it decides what
// is reported about the ones that do. Keeping it off the term list is what
// stops the compiler having to ask whether each predicate is really a
// predicate.
type Stats struct {
	Aggs []Aggregate
	By   []GroupKey
}

// String renders the clause back to DSL text.
func (s *Stats) String() string {
	if s == nil || len(s.Aggs) == 0 {
		return ""
	}

	parts := make([]string, len(s.Aggs))
	for i, a := range s.Aggs {
		parts[i] = a.String()
	}

	out := statsKeyword + " " + strings.Join(parts, ", ")
	if len(s.By) == 0 {
		return out
	}

	keys := make([]string, len(s.By))
	for i, k := range s.By {
		keys[i] = k.String()
	}
	return out + " " + byKeyword + " " + strings.Join(keys, ", ")
}

// HasBin reports whether the grouping buckets time.
func (s *Stats) HasBin() bool {
	if s == nil {
		return false
	}
	for _, k := range s.By {
		if k.IsBin() {
			return true
		}
	}
	return false
}

// BinWidth returns the bucket width, or zero when the grouping has no bin.
func (s *Stats) BinWidth() time.Duration {
	if s == nil {
		return 0
	}
	for _, k := range s.By {
		if k.IsBin() {
			return k.Bin
		}
	}
	return 0
}

// renderAggField writes a field name inside a function call.
//
// The parentheses are the addition. A field name is otherwise rendered by the
// same rule as anywhere else, but inside a call a bare `)` would close the
// call early and a bare `(` would open a second one, so both force quoting.
func renderAggField(name string) string {
	if name == "" {
		return ""
	}
	if aggFieldNeedsQuoting(name) {
		return quote(name)
	}
	return name
}

// renderGroupKey writes a field name in the grouping list.
//
// Parentheses are quoted here too, so that a field genuinely called `bin(1m)`
// renders as `"bin(1m)"` and reads back as a field rather than as a bucket.
func renderGroupKey(name string) string {
	if aggFieldNeedsQuoting(name) {
		return quote(name)
	}
	return name
}

func aggFieldNeedsQuoting(name string) bool {
	return keyNeedsQuoting(name) || strings.ContainsAny(name, "()")
}

// FormatDuration renders a duration in the DSL's own duration grammar, so a
// window reported back to the user is spelled the way they would type it.
//
// time.Duration.String would render a quarter of an hour as 15m0s, which is
// not something the language accepts and not something anybody writes.
func FormatDuration(d time.Duration) string { return renderBinDuration(d) }

// renderBinDuration writes a bin width in the largest unit that divides it
// exactly, so bin(60s) renders as bin(1m) and stays readable.
//
// The parser only admits whole seconds, which is what makes this exact: every
// width it can produce has a form here that parses back to the same duration.
func renderBinDuration(d time.Duration) string {
	units := []struct {
		suffix string
		size   time.Duration
	}{
		{"w", 7 * 24 * time.Hour},
		{"d", 24 * time.Hour},
		{"h", time.Hour},
		{"m", time.Minute},
		{"s", time.Second},
	}

	for _, u := range units {
		if d >= u.size && d%u.size == 0 {
			return strconv.FormatInt(int64(d/u.size), 10) + u.suffix
		}
	}

	// Unreachable through the parser, which rejects anything finer than a
	// second. A hand-built AST still has to render to something that parses.
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + "s"
}

// isReservedFree reports whether a bare word at the start of a term would be
// read as something other than free text, and so has to be rendered in quotes.
//
// Only `stats` qualifies. The time keywords need a colon after them to be
// keywords at all, so a bare `last` is already free text and stays that way.
func isReservedFree(word string) bool {
	return strings.EqualFold(word, statsKeyword)
}
