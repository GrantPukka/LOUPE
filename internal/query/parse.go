package query

import (
	"strings"
)

// Parse turns a filter expression into an AST.
//
// It does not resolve time expressions, look up field names, or touch the
// database. Those need context the parser does not have, and keeping them out
// means a query can be parsed, rendered, and compared without a store.
func Parse(input string) (Query, error) {
	tokens, err := lex(input)
	if err != nil {
		annotate(err, input)
		return Query{}, err
	}

	p := &parser{tokens: tokens, input: input}
	q, err := p.parseQuery()
	if err != nil {
		annotate(err, input)
		return Query{}, err
	}
	return q, nil
}

// annotate attaches the original input to a syntax error so the message can
// point at the offending character.
func annotate(err error, input string) {
	if se, ok := err.(*SyntaxError); ok {
		se.Input = input
	}
}

type parser struct {
	tokens []token
	pos    int
	input  string
}

func (p *parser) peek() token         { return p.tokens[p.pos] }
func (p *parser) next() token         { t := p.tokens[p.pos]; p.pos++; return t }
func (p *parser) atEOF() bool         { return p.peek().kind == tokenEOF }
func (p *parser) at(k tokenKind) bool { return p.peek().kind == k }

func (p *parser) parseQuery() (Query, error) {
	var q Query

	for !p.atEOF() {
		term, err := p.parseTerm()
		if err != nil {
			return Query{}, err
		}
		q.Terms = append(q.Terms, term)
	}

	return q, nil
}

func (p *parser) parseTerm() (Term, error) {
	negate := false
	if p.at(tokenMinus) {
		p.next()
		negate = true

		if p.atEOF() {
			return nil, &SyntaxError{
				Pos:     p.peek().pos,
				Message: "'-' with nothing to negate",
				Hint:    "write the term after it, e.g. -level:debug",
			}
		}
	}

	head := p.next()

	switch head.kind {
	case tokenQuoted:
		return &FreeTerm{Value: Value{Text: head.text, Quoted: true}, Negate: negate}, nil

	case tokenWord:
		// A word introduces a field when followed by a colon, or by a bare ~
		// in the message~pattern form. Anything else is free text.
		switch {
		case p.at(tokenColon):
			p.next()
			return p.parseKeyed(head, negate)
		case p.at(tokenOp):
			return p.parseKeyed(head, negate)
		default:
			return &FreeTerm{Value: Value{Text: head.text}, Negate: negate}, nil
		}

	default:
		return nil, &SyntaxError{
			Pos:     head.pos,
			Message: "unexpected " + head.kind.String(),
			Hint:    "terms look like level:error, status:>=500, or a bare word",
		}
	}
}

// parseKeyed parses everything after `key:`.
func (p *parser) parseKeyed(key token, negate bool) (Term, error) {
	lower := strings.ToLower(key.text)

	// Time keywords take the rest of the term verbatim: their grammar is its
	// own, resolved later against the data's date range and the display
	// timezone.
	if timeKeywords[lower] {
		expr, err := p.parseTimeExpr(lower, key.pos)
		if err != nil {
			return nil, err
		}
		return &TimeTerm{Keyword: lower, Expr: expr, Negate: negate}, nil
	}

	// A bare range with no keyword: 14:00-15:00. The lexer split it on the
	// colon, so the "key" here is the hour. No field is named by a number, so
	// this is unambiguous.
	if isClockHour(key.text) {
		expr, err := p.parseTimeExpr("", key.pos)
		if err != nil {
			return nil, err
		}
		return &TimeTerm{Expr: key.text + ":" + expr, Negate: negate}, nil
	}

	op := OpEq
	if p.at(tokenOp) {
		op = Op(p.next().text)
	}

	values, err := p.parseValues(key, op)
	if err != nil {
		return nil, err
	}

	return &FieldTerm{Key: key.text, Op: op, Values: values, Negate: negate}, nil
}

// parseValues reads a comma-separated value list. Commas mean OR within one
// term; whitespace ends the term.
func (p *parser) parseValues(key token, op Op) ([]Value, error) {
	var values []Value

	for {
		v, err := p.parseValue(key, op)
		if err != nil {
			return nil, err
		}
		values = append(values, v)

		if !p.at(tokenComma) {
			break
		}
		p.next()
	}

	return values, nil
}

func (p *parser) parseValue(key token, op Op) (Value, error) {
	tok := p.next()

	switch tok.kind {
	case tokenWord:
		return Value{Text: tok.text}, nil
	case tokenQuoted:
		return Value{Text: tok.text, Quoted: true}, nil
	case tokenRegex:
		if op != OpMatch {
			return Value{}, &SyntaxError{
				Pos:     tok.pos,
				Message: "a regex value needs the ~ operator",
				Hint:    "write " + key.text + "~/" + tok.text + "/",
			}
		}
		return Value{Text: tok.text, Regex: true}, nil
	default:
		return Value{}, &SyntaxError{
			Pos:     tok.pos,
			Message: "expected a value after " + key.text + ":",
			Hint:    "e.g. " + key.text + ":error, or " + key.text + `:"a phrase"`,
		}
	}
}

// parseTimeExpr reads the expression following a time keyword.
//
// A time expression is taken as a single token rather than being parsed here,
// because its grammar — bare times, ranges, durations, RFC3339 — is resolved
// against the data's date range and the display timezone, neither of which
// exist at parse time.
func (p *parser) parseTimeExpr(keyword string, pos int) (string, error) {
	if p.atEOF() {
		return "", &SyntaxError{
			Pos:     pos,
			Message: "missing time after " + keyword + ":",
			Hint:    timeHint(keyword),
		}
	}

	tok := p.next()
	if tok.kind != tokenWord && tok.kind != tokenQuoted {
		return "", &SyntaxError{
			Pos:     tok.pos,
			Message: "expected a time after " + keyword + ":",
			Hint:    timeHint(keyword),
		}
	}

	expr := tok.text

	// A bare time contains colons, which the lexer split. Reassemble them:
	// after:14:00:00 arrives as "14", ":", "00", ":", "00".
	for p.at(tokenColon) {
		p.next()
		if !p.at(tokenWord) {
			return "", &SyntaxError{
				Pos:     p.peek().pos,
				Message: "incomplete time after " + keyword + ":",
				Hint:    timeHint(keyword),
			}
		}
		expr += ":" + p.next().text
	}

	return expr, nil
}

// isClockHour reports whether a token is one or two digits, and so is the hour
// of a bare time range rather than a field name.
//
// Note that a bare range must contain a colon to be recognised. 1400-1500 is
// left as free text: it is a plausible time range but an equally plausible pair
// of error codes, and between:1400-1500 says it unambiguously.
func isClockHour(s string) bool {
	if len(s) == 0 || len(s) > 2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func timeHint(keyword string) string {
	switch keyword {
	case "last":
		return "e.g. last:15m, last:2h, last:3d"
	case "on":
		return "e.g. on:2026-08-13"
	case "between":
		return "e.g. between:14:00-15:00"
	default:
		return "e.g. " + keyword + ":14:00 or " + keyword + ":2026-08-13T14:00:00Z"
	}
}
