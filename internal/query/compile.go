package query

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/GrantPukka/loupe/internal/parse"
)

// SQL is a compiled query: a WHERE clause and its parameters.
//
// The two travel together and must never be separated. Every value that came
// from user input is in Args; nothing from the query string is interpolated
// into Where.
type SQL struct {
	Where string
	Args  []any
}

// builder accumulates predicates and their parameters.
type builder struct {
	schema Schema
	preds  []string
	args   []any
}

func (b *builder) arg(v any) string {
	b.args = append(b.args, v)
	return "?"
}

// Compile turns a parsed query into parameterised SQL.
//
// Time terms are not compiled here. They must be intersected into a single
// interval against the data's date range and the display timezone first, which
// is the caller's job; a query still carrying unresolved time terms is a
// programming error and says so.
func Compile(q Query, schema Schema) (SQL, error) {
	b := &builder{schema: schema}

	for _, term := range q.Terms {
		var pred string
		var err error

		switch t := term.(type) {
		case *FieldTerm:
			pred, err = b.fieldTerm(t)
		case *FreeTerm:
			pred, err = b.freeTerm(t)
		case *ResolvedTimeTerm:
			pred = b.timeTerm(t)
		case *TimeTerm:
			err = &UnresolvedTimeError{Term: t.String()}
		default:
			err = fmt.Errorf("cannot compile %T", term)
		}

		if err != nil {
			return SQL{}, err
		}
		if pred != "" {
			b.preds = append(b.preds, pred)
		}
	}

	if len(b.preds) == 0 {
		return SQL{Where: "TRUE"}, nil
	}
	return SQL{Where: strings.Join(b.preds, " AND "), Args: b.args}, nil
}

// UnresolvedTimeError means a time term reached the compiler without being
// resolved to an interval.
//
// This is a programming error rather than a user error: the caller forgot to
// run ResolveTime. It is reported rather than ignored because silently dropping
// a time term would widen the query without telling anybody.
type UnresolvedTimeError struct{ Term string }

func (e *UnresolvedTimeError) Error() string {
	return fmt.Sprintf("internal: time term %q reached the compiler unresolved; "+
		"ResolveTime must run before Compile", e.Term)
}

// timeTerm compiles the resolved window.
//
// It becomes ts >= ? AND ts < ?, which is index-friendly and, importantly,
// excludes records with a NULL timestamp automatically. Those records are not
// lost — ts:none selects them, and the caller reports how many a time filter
// left out — but they cannot honestly be said to fall inside a window.
func (b *builder) timeTerm(t *ResolvedTimeTerm) string {
	var parts []string

	if !t.Interval.Start.IsZero() {
		parts = append(parts, "ts >= "+b.arg(t.Interval.Start.UTC()))
	}
	if !t.Interval.End.IsZero() {
		parts = append(parts, "ts < "+b.arg(t.Interval.End.UTC()))
	}

	for _, ex := range t.Exclude {
		var bounds []string
		if !ex.Start.IsZero() {
			bounds = append(bounds, "ts >= "+b.arg(ex.Start.UTC()))
		}
		if !ex.End.IsZero() {
			bounds = append(bounds, "ts < "+b.arg(ex.End.UTC()))
		}
		if len(bounds) > 0 {
			parts = append(parts, negate("("+strings.Join(bounds, " AND ")+")"))
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " AND ")
}

// negate wraps a predicate so that it excludes matches without also excluding
// records where the expression is NULL.
//
// This matters more than it looks. In SQL, NOT (level = 'debug') is NULL for a
// record with no level, and a NULL predicate does not match — so the obvious
// compilation of -level:debug silently hides every record that has no level at
// all. The user asked for everything except debug, and would get neither an
// error nor the records.
func negate(pred string) string {
	return "NOT coalesce(" + pred + ", FALSE)"
}

func (b *builder) fieldTerm(t *FieldTerm) (string, error) {
	pred, err := b.fieldPredicate(t)
	if err != nil {
		return "", err
	}
	if t.Negate {
		return negate(pred), nil
	}
	return pred, nil
}

func (b *builder) fieldPredicate(t *FieldTerm) (string, error) {
	// Existence tests are the same for every key, promoted or not.
	if len(t.Values) == 1 && t.Op == OpEq {
		switch strings.ToLower(t.Values[0].Text) {
		case "none":
			expr, err := b.schema.resolve(t.Key)
			if err != nil {
				return "", err
			}
			return "(" + expr + ") IS NULL", nil
		case "*":
			expr, err := b.schema.resolve(t.Key)
			if err != nil {
				return "", err
			}
			return "(" + expr + ") IS NOT NULL", nil
		}
	}

	switch strings.ToLower(t.Key) {
	case "level":
		return b.levelTerm(t)
	case "source":
		return b.sourceTerm(t)
	case "file":
		return b.fileTerm(t)
	}

	expr, err := b.schema.resolve(t.Key)
	if err != nil {
		return "", err
	}

	return b.valuePredicates(expr, t)
}

// valuePredicates ORs the term's values together. Commas within a value mean
// OR; that is the only disjunction the language has.
func (b *builder) valuePredicates(expr string, t *FieldTerm) (string, error) {
	parts := make([]string, 0, len(t.Values))

	for _, v := range t.Values {
		pred, err := b.comparison(expr, t.Op, v)
		if err != nil {
			return "", err
		}
		parts = append(parts, pred)
	}

	return orTogether(parts), nil
}

func orTogether(parts []string) string {
	if len(parts) == 1 {
		return parts[0]
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// comparison builds one predicate.
func (b *builder) comparison(expr string, op Op, v Value) (string, error) {
	switch op {
	case OpEq:
		return b.equality(expr, v), nil

	case OpMatch:
		if v.Regex {
			return fmt.Sprintf("regexp_matches(%s, %s)", expr, b.arg(regexFlags(v.Text)+v.Text)), nil
		}
		return b.contains(expr, v.Text), nil

	case OpGT, OpLT, OpGE, OpLE:
		return b.ordered(expr, op, v)

	default:
		return "", fmt.Errorf("unsupported operator %q", op)
	}
}

// equality compares as text. Values arrive from the DSL as strings and the
// stored JSON extraction is text, so a string comparison is the honest one; a
// numeric field still compares correctly because both sides render the same
// way.
func (b *builder) equality(expr string, v Value) string {
	return fmt.Sprintf("%s = %s", expr, b.arg(v.Text))
}

// ordered builds a comparison.
//
// When the written value is numeric, both sides are cast so that 9 does not
// sort above 10. TRY_CAST yields NULL for a value that is not a number, which
// correctly excludes it rather than failing the whole query — a single
// non-numeric latency_ms in a million records must not error the query out.
func (b *builder) ordered(expr string, op Op, v Value) (string, error) {
	if _, err := strconv.ParseFloat(v.Text, 64); err == nil {
		return fmt.Sprintf("TRY_CAST(%s AS DOUBLE) %s %s", expr, op, b.arg(mustFloat(v.Text))), nil
	}
	return fmt.Sprintf("%s %s %s", expr, op, b.arg(v.Text)), nil
}

func mustFloat(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// contains builds a substring match with smart case: case-insensitive unless
// the pattern contains an uppercase character.
//
// That is ripgrep's behaviour, which users of this kind of tool already have in
// their fingers.
//
// The case-insensitive form is a literal-anchored regex rather than
// lower(expr) LIKE …, which is what it used to be. DuckDB's lower() does not
// merely reject text that is not valid UTF-8, it fails to return on it, so a
// single corrupt byte anywhere in the corpus made every lowercase search — the
// primary interaction of the whole tool — hang forever with no output, while
// the same search with one capital letter in it returned instantly. Ingest no
// longer stores such text (see store.sanitiseEntry), but the tool should not
// depend on that to answer a query at all, and RE2 measures marginally faster
// here besides.
func (b *builder) contains(expr, needle string) string {
	if hasUpper(needle) {
		return fmt.Sprintf("%s LIKE %s", expr, b.arg("%"+escapeLike(needle)+"%"))
	}
	return fmt.Sprintf("regexp_matches(%s, %s)", expr, b.arg("(?i)"+regexp.QuoteMeta(needle)))
}

// regexFlags prefixes a case-insensitive flag unless the pattern contains an
// uppercase character, matching the smart-case rule used for substrings.
func regexFlags(pattern string) string {
	if hasUpper(pattern) {
		return ""
	}
	return "(?i)"
}

func hasUpper(s string) bool {
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			return true
		}
	}
	return false
}

// escapeLike neutralises LIKE wildcards in a literal substring, so that
// searching for 100% finds the text rather than matching everything.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// levelTerm compiles a severity comparison.
//
// Ordinal comparisons expand to an explicit list rather than a rank lookup in
// SQL, which keeps the predicate index-friendly and, more importantly, makes
// the unranked case correct for free: a level outside the canonical set is
// simply not in the expansion, so level:>=warn cannot sweep up a source's
// custom "audit" level.
func (b *builder) levelTerm(t *FieldTerm) (string, error) {
	if t.Op == OpMatch {
		return b.valuePredicates("level", t)
	}

	var wanted []string
	seen := map[string]bool{}

	for _, v := range t.Values {
		level := parse.NormaliseLevel(v.Text)

		if t.Op == OpEq {
			if !seen[level] {
				seen[level] = true
				wanted = append(wanted, level)
			}
			continue
		}

		rank, ranked := parse.LevelRank(level)
		if !ranked {
			return "", &UnknownLevelError{Value: v.Text, Op: string(t.Op)}
		}

		for i, candidate := range parse.Levels {
			if !inRange(i, rank, t.Op) || seen[candidate] {
				continue
			}
			seen[candidate] = true
			wanted = append(wanted, candidate)
		}
	}

	if len(wanted) == 0 {
		return "FALSE", nil
	}

	placeholders := make([]string, len(wanted))
	for i, level := range wanted {
		placeholders[i] = b.arg(level)
	}
	return "level IN (" + strings.Join(placeholders, ", ") + ")", nil
}

func inRange(i, rank int, op Op) bool {
	switch op {
	case OpGE:
		return i >= rank
	case OpGT:
		return i > rank
	case OpLE:
		return i <= rank
	case OpLT:
		return i < rank
	default:
		return i == rank
	}
}

// UnknownLevelError reports a comparison against a level that has no rank.
type UnknownLevelError struct {
	Value string
	Op    string
}

func (e *UnknownLevelError) Error() string {
	return fmt.Sprintf("cannot compare level:%s%s — %q is not one of %s\n"+
		"a level outside that set matches only on exact equality, so try level:%s",
		e.Op, e.Value, e.Value, strings.Join(parse.Levels, ", "), e.Value)
}

// sourceTerm compiles a source filter, expanding unambiguous prefixes.
func (b *builder) sourceTerm(t *FieldTerm) (string, error) {
	if t.Op != OpEq {
		return b.valuePredicates("source", t)
	}

	parts := make([]string, 0, len(t.Values))
	for _, v := range t.Values {
		name, err := b.schema.resolveSource(v.Text)
		if err != nil {
			return "", err
		}
		parts = append(parts, "source = "+b.arg(name))
	}
	return orTogether(parts), nil
}

// fileTerm compiles a file filter.
//
// A file column holds a path, but people type a base name, so both are matched.
// Globs are supported because rotated logs are the reason anyone types this:
// file:access.log* catching access.log.1 and access.log.2.gz is the whole point.
func (b *builder) fileTerm(t *FieldTerm) (string, error) {
	if t.Op != OpEq {
		return b.valuePredicates("file", t)
	}

	parts := make([]string, 0, len(t.Values))
	for _, v := range t.Values {
		if strings.ContainsAny(v.Text, "*?[") {
			parts = append(parts, fmt.Sprintf("(file GLOB %s OR file GLOB %s)",
				b.arg(v.Text), b.arg("*/"+v.Text)))
			continue
		}
		parts = append(parts, fmt.Sprintf("(file = %s OR file LIKE %s)",
			b.arg(v.Text), b.arg("%/"+escapeLike(v.Text))))
	}
	return orTogether(parts), nil
}

// freeTerm compiles a bare word or quoted phrase.
//
// Bare words search the message and every field value, because that is what
// people expect from a search box. Note that the fields bag is matched as its
// JSON text, so a key name can match as well as a value — an over-match that
// costs a user nothing compared with the alternative of missing a hit.
//
// Raw is included so that a line no parser understood is still findable. Those
// records are exactly the ones someone is hunting for when the tool has
// otherwise let them down.
func (b *builder) freeTerm(t *FreeTerm) (string, error) {
	needle := t.Value.Text
	if needle == "" {
		return "", nil
	}

	pred := "(" + strings.Join([]string{
		b.contains("message", needle),
		b.contains("coalesce(fields, '')", needle),
		b.contains("raw", needle),
	}, " OR ") + ")"

	if t.Negate {
		return negate(pred), nil
	}
	return pred, nil
}

// StatsColumn is one output column of an aggregation.
type StatsColumn struct {
	// Name is the column's DSL text, used as its heading. A heading that is
	// the thing you would type to ask the question again is worth more than a
	// prettier one.
	Name string
	// Expr is the SQL that produces the column.
	Expr string
	// Group is true for a grouping column, false for an aggregate.
	Group bool
	// Bin is true for the grouping column that buckets time.
	Bin bool
	// Present is the condition a record must satisfy to belong to a group at
	// all, set only on grouping columns. A record with no value for a grouping
	// field has no group to sit in; excluding it is unavoidable, so the count
	// of what was excluded travels back to the caller to be reported.
	Present string
}

// StatsNumeric names a field a numeric aggregate reads.
//
// The caller probes these before running the aggregation, because a field that
// holds no numbers must produce an error rather than a column of NULLs. A
// summary that quietly reports nothing where it should report a refusal is the
// confident wrong answer this project exists to avoid.
type StatsNumeric struct {
	// Field is the name as written, for the error message.
	Field string
	// Agg is the first aggregate reading it, e.g. avg(latency_ms).
	Agg string
	// Expr is the SQL that reads the raw value, before any cast.
	Expr string
}

// StatsSQL is a compiled aggregation clause: everything except the filter's
// own WHERE, which Compile produces separately and the caller ANDs in.
//
// No parameters. Every value in a stats clause is a field name or a bucket
// width: a field name is resolved to a column expression by the schema, and a
// width is a number this package computed. Nothing from the query string is
// interpolated, which is why Args does not appear here at all.
type StatsSQL struct {
	// Select is the output columns, grouping columns first.
	Select []StatsColumn
	// GroupBy and OrderBy are clauses, empty when there is nothing to group.
	GroupBy string
	OrderBy string
	// Numeric is the fields to probe before running the aggregation.
	Numeric []StatsNumeric
	// Bin is the bucket width when the grouping includes one.
	Bin time.Duration
	// Origin is what the buckets are anchored to.
	Origin time.Time
}

// StatsOptions is what compiling an aggregation needs beyond the schema.
type StatsOptions struct {
	// Origin anchors time bins: a bucket boundary falls on Origin plus a whole
	// multiple of the bin width. The caller passes local midnight in the
	// display timezone, so bin(1h) lands on the hour on the user's clock
	// rather than on the hour in UTC.
	Origin time.Time
}

// CompileStats turns an aggregation clause into the SQL that answers it.
//
// Grouping columns come first and aggregates after, which is the order every
// stats table has been printed in since the invention of the stats table.
func CompileStats(s *Stats, schema Schema, opts StatsOptions) (StatsSQL, error) {
	if s == nil || len(s.Aggs) == 0 {
		return StatsSQL{}, fmt.Errorf("internal: no aggregates to compile")
	}

	out := StatsSQL{Bin: s.BinWidth(), Origin: opts.Origin}

	for _, key := range s.By {
		col, err := statsGroupColumn(key, schema, opts)
		if err != nil {
			return StatsSQL{}, err
		}
		out.Select = append(out.Select, col)
	}

	for _, agg := range s.Aggs {
		expr, err := statsAggExpr(agg, schema)
		if err != nil {
			return StatsSQL{}, err
		}
		out.Select = append(out.Select, StatsColumn{Name: agg.String(), Expr: expr})
		out.Numeric = appendNumeric(out.Numeric, agg, schema)
	}

	out.GroupBy, out.OrderBy = statsClauses(s)
	return out, nil
}

// statsGroupColumn compiles one grouping.
func statsGroupColumn(key GroupKey, schema Schema, opts StatsOptions) (StatsColumn, error) {
	if key.IsBin() {
		return StatsColumn{
			Name:  key.String(),
			Expr:  binExpr(key.Bin, opts.Origin),
			Group: true,
			Bin:   true,
			// A record with no timestamp belongs in no bucket. ts:none still
			// finds it, and the caller states how many there were, which is
			// what docs/FILTER-DSL.md section 2.4 requires of anything that
			// filters on time.
			Present: "ts IS NOT NULL",
		}, nil
	}

	expr, err := schema.resolve(key.Field)
	if err != nil {
		return StatsColumn{}, err
	}
	return StatsColumn{
		Name:    key.String(),
		Expr:    expr,
		Group:   true,
		Present: "(" + expr + ") IS NOT NULL",
	}, nil
}

// statsAggExpr compiles one aggregate.
//
// The function name reaches SQL as text, which is safe for exactly one reason:
// AggFunc values come from a closed lookup table in the parser, never from the
// query string. A name that is not in that table has already been refused with
// a spelling suggestion.
func statsAggExpr(agg Aggregate, schema Schema) (string, error) {
	if agg.Func == AggCount && agg.Field == "" {
		return "count(*)", nil
	}

	expr, err := schema.resolve(agg.Field)
	if err != nil {
		return "", err
	}

	// count(field) counts the records that carry the field, whatever it holds.
	// It is the one aggregate that does not need a number.
	if agg.Func == AggCount {
		return "count(" + expr + ")", nil
	}

	// TRY_CAST yields NULL for a value that is not a number, which excludes it
	// rather than failing the whole query — the same rule the ordering
	// comparisons use. How many were excluded is reported, so an average over
	// two thirds of the values never passes as an average over all of them.
	num := "TRY_CAST(" + expr + " AS DOUBLE)"

	if q, ok := quantiles[agg.Func]; ok {
		return fmt.Sprintf("quantile_cont(%s, %s)",
			num, strconv.FormatFloat(q, 'f', -1, 64)), nil
	}
	return string(agg.Func) + "(" + num + ")", nil
}

// binExpr buckets the timestamp.
//
// The width and the origin are numbers computed here, never user text, and are
// formatted into the statement because DuckDB accepts no placeholder in an
// INTERVAL or a TIMESTAMP literal. This is the same bargain internal/session's
// histogram makes, for the same reason.
func binExpr(width time.Duration, origin time.Time) string {
	return fmt.Sprintf("time_bucket(INTERVAL '%d' MICROSECOND, ts, TIMESTAMP '%s')",
		width.Microseconds(), origin.UTC().Format("2006-01-02 15:04:05.999999"))
}

// appendNumeric records a field that has to hold numbers, once per field.
func appendNumeric(list []StatsNumeric, agg Aggregate, schema Schema) []StatsNumeric {
	if !agg.Func.Numeric() {
		return list
	}
	for _, n := range list {
		if n.Field == agg.Field {
			return list
		}
	}

	// resolve already succeeded in statsAggExpr, so this cannot fail.
	expr, err := schema.resolve(agg.Field)
	if err != nil {
		return list
	}
	return append(list, StatsNumeric{Field: agg.Field, Agg: agg.String(), Expr: expr})
}

// statsClauses builds GROUP BY and ORDER BY from column positions.
//
// Positions rather than names: two aggregates can render to the same text —
// `stats count(), count()` is legal if pointless — and an ambiguous ORDER BY
// would be an error rather than a listing.
//
// Ordering is by time when the grouping has a bin, because a rate over time
// read in any other order is not a rate over time. Otherwise it is by the
// first aggregate, largest first, which puts the answer to "which is worst" on
// the first line. Group columns break ties so that the same data always lists
// in the same order.
func statsClauses(s *Stats) (groupBy, orderBy string) {
	if len(s.By) == 0 {
		return "", ""
	}

	positions := make([]string, len(s.By))
	for i := range s.By {
		positions[i] = strconv.Itoa(i + 1)
	}
	groupBy = "GROUP BY " + strings.Join(positions, ", ")

	if bin := binPosition(s); bin > 0 {
		order := []string{strconv.Itoa(bin)}
		for i := range s.By {
			if i+1 != bin {
				order = append(order, strconv.Itoa(i+1))
			}
		}
		return groupBy, "ORDER BY " + strings.Join(order, ", ")
	}

	first := strconv.Itoa(len(s.By) + 1)
	return groupBy, "ORDER BY " + first + " DESC NULLS LAST, " + strings.Join(positions, ", ")
}

// binPosition is the 1-based column of the time bucket, or zero if there is
// none.
func binPosition(s *Stats) int {
	for i, key := range s.By {
		if key.IsBin() {
			return i + 1
		}
	}
	return 0
}

// SelectItem is the column's SQL with its heading as an alias.
//
// The alias is the DSL text of the column, so the table's headings are the
// words that produced them and a reader can retype the question from the
// output. Quoting goes through the same identifier quoter the schema uses, so
// a field named `order` or one containing a quote cannot break the statement.
func (c StatsColumn) SelectItem() string {
	return "(" + c.Expr + ") AS " + quoteIdent(c.Name)
}
