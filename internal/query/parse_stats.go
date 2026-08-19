package query

import (
	"fmt"
	"strings"
	"time"
)

// atStats reports whether the parser is at the start of an aggregation clause.
//
// `stats` is a keyword only where a term would begin, and only when it is not
// being used as a field name. `stats:5` and `stats~high` are ordinary field
// terms on a field called stats, which is what a log file that counts things
// will produce.
func (p *parser) atStats() bool {
	tok := p.peek()
	if tok.kind != tokenWord || !strings.EqualFold(tok.text, statsKeyword) {
		return false
	}

	// Safe to look ahead: the current token is a word, so it is not the EOF
	// token, and the EOF token is always last.
	switch p.tokens[p.pos+1].kind {
	case tokenColon, tokenOp:
		return false
	}
	return true
}

// parseStats reads `stats <aggregates> [by <keys>]`.
//
// The clause is not resolved here any more than a time term is: whether a
// field exists, and whether it holds numbers, needs the loaded data.
func (p *parser) parseStats() (*Stats, error) {
	kw := p.next()

	out := &Stats{}
	for {
		agg, err := p.parseAggregate(kw)
		if err != nil {
			return nil, err
		}
		out.Aggs = append(out.Aggs, agg)

		if !p.at(tokenComma) {
			break
		}
		p.next()
	}

	if !p.at(tokenWord) || !strings.EqualFold(p.peek().text, byKeyword) {
		return out, nil
	}
	by := p.next()

	for {
		key, err := p.parseGroupKey(by)
		if err != nil {
			return nil, err
		}
		out.By = append(out.By, key)

		if !p.at(tokenComma) {
			break
		}
		p.next()
	}

	return out, nil
}

// parseAggregate reads one `func(field)` call.
//
// The lexer does not know about parentheses, and deliberately so: making them
// structural everywhere would break every value that contains one, and log
// messages are full of them. `count(latency_ms)` therefore arrives as a single
// word and is taken apart here, which is the same division of labour that
// leaves a time expression whole for the resolver.
func (p *parser) parseAggregate(kw token) (Aggregate, error) {
	tok := p.peek()

	if tok.kind != tokenWord || !strings.Contains(tok.text, "(") {
		return Aggregate{}, &SyntaxError{
			Pos:     tok.pos,
			Message: aggMissingMessage(kw, tok),
			Hint:    aggHint,
		}
	}
	p.next()

	name, rest, _ := strings.Cut(tok.text, "(")
	fn, err := parseAggFunc(name, tok.pos)
	if err != nil {
		return Aggregate{}, err
	}

	field, err := p.parseAggField(fn, rest, tok)
	if err != nil {
		return Aggregate{}, err
	}
	return Aggregate{Func: fn, Field: field}, nil
}

// parseAggField reads what stands between the parentheses.
//
// rest is everything the word held after the opening bracket. It is empty when
// the field name was quoted, because the lexer ends a bare word at a quote —
// `p99("odd key")` arrives as three tokens, and a field name containing a
// space, a colon or a bracket has no other way to be written.
func (p *parser) parseAggField(fn AggFunc, rest string, tok token) (string, error) {
	field, err := p.aggFieldText(fn, rest, tok)
	if err != nil {
		return "", err
	}

	// count(*) is what people type for "every record", and means the same as
	// count(). Every other function needs something to read.
	if fn == AggCount && field == "*" {
		field = ""
	}
	if fn != AggCount && field == "" {
		return "", &SyntaxError{
			Pos:     tok.pos,
			Message: string(fn) + "() needs a field to read",
			Hint:    "e.g. " + string(fn) + "(latency_ms); only count() works over every record",
		}
	}
	return field, nil
}

// aggFieldText reads the field name out of a call, quoted or bare.
func (p *parser) aggFieldText(fn AggFunc, rest string, tok token) (string, error) {
	if rest == "" {
		return p.parseQuotedAggField(fn, tok)
	}

	trimmed, ok := strings.CutSuffix(rest, ")")
	if !ok || strings.ContainsAny(trimmed, "()") {
		return "", &SyntaxError{
			Pos:     tok.pos,
			Message: fmt.Sprintf("%s is not a complete aggregate", tok.text),
			Hint:    "close the brackets and leave no spaces inside them, e.g. " + string(fn) + "(latency_ms)",
		}
	}
	return trimmed, nil
}

// parseQuotedAggField reads the `"name")` that follows an opening bracket.
func (p *parser) parseQuotedAggField(fn AggFunc, tok token) (string, error) {
	if !p.at(tokenQuoted) {
		return "", &SyntaxError{
			Pos:     p.peek().pos,
			Message: fmt.Sprintf("%s is not a complete aggregate", tok.text),
			Hint:    "close the brackets and leave no spaces inside them, e.g. " + string(fn) + "(latency_ms)",
		}
	}
	name := p.next()

	if !p.at(tokenWord) || p.peek().text != ")" {
		return "", &SyntaxError{
			Pos:     p.peek().pos,
			Message: "missing ) after " + string(fn) + "(" + quote(name.text),
			Hint:    "e.g. " + string(fn) + `("a field name with spaces")`,
		}
	}
	p.next()

	return name.text, nil
}

// parseAggFunc resolves a written function name.
//
// An unknown name is an error with a suggestion, for the same reason an unknown
// field is: `mean(latency_ms)` returning nothing, or worse returning zeroes,
// teaches the user that the data is empty rather than that the word is wrong.
func parseAggFunc(name string, pos int) (AggFunc, error) {
	fn, ok := aggFuncs[strings.ToLower(name)]
	if ok {
		return fn, nil
	}

	msg := "unknown aggregate " + name + "()"
	if name == "" {
		msg = "an aggregate needs a function name"
	}
	return "", &SyntaxError{
		Pos:     pos,
		Message: msg,
		Hint:    aggSuggestion(name),
	}
}

// parseGroupKey reads one entry of the `by` list: a field, or a time bucket.
func (p *parser) parseGroupKey(by token) (GroupKey, error) {
	tok := p.peek()

	switch tok.kind {
	case tokenQuoted:
		p.next()
		return GroupKey{Field: tok.text}, nil

	case tokenWord:
		p.next()

		if inner, ok := binArgument(tok.text); ok {
			d, err := parseBin(inner, tok.pos)
			if err != nil {
				return GroupKey{}, err
			}
			return GroupKey{Bin: d}, nil
		}

		// Anything else carrying brackets is an aggregate in the wrong half of
		// the clause, which is worth saying rather than reading as a field
		// name nobody has.
		if strings.ContainsAny(tok.text, "()") {
			return GroupKey{}, &SyntaxError{
				Pos:     tok.pos,
				Message: tok.text + " cannot be grouped by",
				Hint: "aggregates go before `by`; to bucket time write bin(1m), " +
					"and for a field whose name has brackets, quote it",
			}
		}
		return GroupKey{Field: tok.text}, nil

	default:
		return GroupKey{}, &SyntaxError{
			Pos:     tok.pos,
			Message: "expected a field after " + by.text,
			Hint:    "e.g. by level, by path, or by bin(1m)",
		}
	}
}

// binArgument reports the text inside a `bin(...)` grouping.
func binArgument(word string) (string, bool) {
	const open = "bin("
	if len(word) < len(open) || !strings.EqualFold(word[:len(open)], open) {
		return "", false
	}
	inner, ok := strings.CutSuffix(word[len(open):], ")")
	if !ok || strings.ContainsAny(inner, "()") {
		return "", false
	}
	return inner, true
}

// parseBin reads a bucket width.
//
// Whole seconds only. Anything finer cannot be written back down — the
// duration grammar's smallest unit is the second — and a bin that rendered as
// something the parser read differently would change the query under the user
// the first time the UI wrote it back into the box.
func parseBin(expr string, pos int) (time.Duration, error) {
	d, err := ParseDuration(expr)
	if err != nil {
		return 0, &SyntaxError{
			Pos:     pos,
			Message: "bin(" + expr + ") is not a bucket width: " + err.Error(),
			Hint:    binHint,
		}
	}
	if d <= 0 {
		return 0, &SyntaxError{
			Pos:     pos,
			Message: "bin(" + expr + ") has no width",
			Hint:    binHint,
		}
	}
	if d%time.Second != 0 {
		return 0, &SyntaxError{
			Pos:     pos,
			Message: "bin(" + expr + ") is finer than a second",
			Hint:    "a bucket is a whole number of seconds, e.g. bin(1s), bin(30s), bin(5m)",
		}
	}
	return d, nil
}

const (
	aggHint = "an aggregate looks like count(), p99(latency_ms), or avg(bytes); " +
		"to search for the word instead, quote it: \"stats\""
	binHint = "a time bucket looks like bin(1m), bin(30s), bin(1h)"
)

// aggMissingMessage names what was found where an aggregate was expected.
func aggMissingMessage(kw, tok token) string {
	switch {
	case tok.kind == tokenEOF:
		return kw.text + " needs at least one aggregate"
	case tok.kind == tokenWord && strings.EqualFold(tok.text, byKeyword):
		return kw.text + " needs an aggregate before " + tok.text
	case tok.kind == tokenWord:
		return tok.text + " is not an aggregate — it has no brackets"
	default:
		return "expected an aggregate after " + kw.text
	}
}

// aggSuggestion lists the functions, closest first.
func aggSuggestion(name string) string {
	names := make([]string, len(AggFuncs))
	for i, f := range AggFuncs {
		names[i] = string(f)
	}

	var sb strings.Builder
	if matches := suggest(name, names); len(matches) > 0 {
		fmt.Fprintf(&sb, "did you mean: %s\n", strings.Join(withBrackets(matches), ", "))
	}
	fmt.Fprintf(&sb, "aggregates: %s", strings.Join(withBrackets(names), ", "))
	return sb.String()
}

func withBrackets(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = n + "()"
	}
	return out
}
