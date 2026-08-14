package query

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// tokenKind is the lexical class of a token.
type tokenKind int

const (
	tokenEOF tokenKind = iota
	// tokenWord is a bare run of characters: a key, a value, or a free-text
	// word. Which one it is, is the parser's problem, not the lexer's.
	tokenWord
	// tokenQuoted is a double-quoted string with its quotes removed.
	tokenQuoted
	// tokenRegex is a /slash-delimited/ pattern with its delimiters removed.
	tokenRegex
	tokenColon
	tokenComma
	tokenMinus
	// tokenOp is a comparison operator: >=, <=, >, <, or ~.
	tokenOp
)

func (k tokenKind) String() string {
	switch k {
	case tokenEOF:
		return "end of input"
	case tokenWord:
		return "word"
	case tokenQuoted:
		return "quoted string"
	case tokenRegex:
		return "regex"
	case tokenColon:
		return "':'"
	case tokenComma:
		return "','"
	case tokenMinus:
		return "'-'"
	case tokenOp:
		return "operator"
	default:
		return "unknown"
	}
}

// token is one lexical unit, carrying its position for error messages that can
// point at the offending character.
type token struct {
	kind tokenKind
	text string
	pos  int
}

// lex splits a filter expression into tokens.
//
// The lexer is deliberately ignorant of meaning. It does not know that level is
// a field or that after is a time keyword; it produces words, punctuation, and
// operators, and the parser assigns significance. Keeping the split here is
// what stops the grammar leaking into character handling.
func lex(input string) ([]token, error) {
	var tokens []token
	i := 0

	for i < len(input) {
		r, size := utf8.DecodeRuneInString(input[i:])

		switch {
		case unicode.IsSpace(r):
			i += size

		case r == ':':
			tokens = append(tokens, token{kind: tokenColon, text: ":", pos: i})
			i++

		case r == ',':
			tokens = append(tokens, token{kind: tokenComma, text: ",", pos: i})
			i++

		case r == '-' && startsTerm(input, i, tokens):
			// A minus only negates at the start of a term. Inside a value it is
			// an ordinary character, so eu-west-1 and 2026-08-13 lex as one
			// word rather than three.
			tokens = append(tokens, token{kind: tokenMinus, text: "-", pos: i})
			i++

		case r == '"':
			text, next, err := lexQuoted(input, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, token{kind: tokenQuoted, text: text, pos: i})
			i = next

		case r == '/' && afterOp(tokens):
			// Slashes only start a regex directly after a ~ operator.
			// Everywhere else they are path characters, so /api/checkout stays
			// one word.
			text, next, err := lexRegex(input, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, token{kind: tokenRegex, text: text, pos: i})
			i = next

		case r == '~' && afterKey(tokens):
			// docs/FILTER-DSL.md section 5 writes the match operator without a
			// colon — message~timeout — while the formal grammar in section 1
			// includes one. Both are accepted; the examples are what people
			// actually type.
			tokens = append(tokens, token{kind: tokenOp, text: "~", pos: i})
			i++

		case isOpStart(r) && afterColon(tokens):
			// Comparison operators only count immediately after a colon.
			// Elsewhere > and < are ordinary characters in a message.
			op, next := lexOp(input, i)
			tokens = append(tokens, token{kind: tokenOp, text: op, pos: i})
			i = next

		default:
			text, next := lexWord(input, i)
			tokens = append(tokens, token{kind: tokenWord, text: text, pos: i})
			i = next
		}
	}

	tokens = append(tokens, token{kind: tokenEOF, pos: len(input)})
	return tokens, nil
}

// startsTerm reports whether a minus at position i begins a new term, and so
// means negation, rather than being an ordinary character inside a value.
//
// Whitespace is the whole distinction. In `region:eu-west-1` the hyphens are
// part of the value; in `level:error -source:nginx` the minus negates. Deciding
// on the preceding token alone is not enough, because in both cases that token
// is a word — a bug that round-trip tests cannot catch, since they misparse the
// input and the rendering identically.
func startsTerm(input string, i int, tokens []token) bool {
	if i == 0 {
		return true
	}

	prev, _ := utf8.DecodeLastRuneInString(input[:i])
	if !unicode.IsSpace(prev) {
		return false
	}

	// After a colon, comma, or operator, a minus is part of the value:
	// `after:-1h` and `offset:-5` are values, not negations.
	if len(tokens) > 0 {
		switch tokens[len(tokens)-1].kind {
		case tokenColon, tokenComma, tokenOp:
			return false
		}
	}
	return true
}

func afterColon(tokens []token) bool {
	return len(tokens) > 0 && tokens[len(tokens)-1].kind == tokenColon
}

// afterKey reports whether the previous token could be a field name, which is
// where a bare ~ is the match operator rather than a character in a word.
func afterKey(tokens []token) bool {
	return len(tokens) > 0 && tokens[len(tokens)-1].kind == tokenWord
}

func afterOp(tokens []token) bool {
	return len(tokens) > 0 && tokens[len(tokens)-1].kind == tokenOp
}

func isOpStart(r rune) bool {
	return r == '>' || r == '<' || r == '~' || r == '='
}

// lexOp reads a comparison operator, normalising = and == to equality by
// returning an empty operator.
func lexOp(input string, i int) (string, int) {
	if i+1 < len(input) && input[i+1] == '=' {
		switch input[i] {
		case '>':
			return ">=", i + 2
		case '<':
			return "<=", i + 2
		case '=':
			return "", i + 2
		}
	}
	switch input[i] {
	case '=':
		return "", i + 1
	default:
		return input[i : i+1], i + 1
	}
}

// lexQuoted reads a double-quoted string, honouring backslash escapes.
func lexQuoted(input string, i int) (string, int, error) {
	var sb strings.Builder
	i++ // opening quote

	for i < len(input) {
		switch input[i] {
		case '\\':
			if i+1 >= len(input) {
				return "", 0, &SyntaxError{
					Pos:     i,
					Message: "trailing backslash inside a quoted string",
					Hint:    `write \\ for a literal backslash`,
				}
			}
			switch input[i+1] {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			default:
				sb.WriteByte(input[i+1])
			}
			i += 2
		case '"':
			return sb.String(), i + 1, nil
		default:
			sb.WriteByte(input[i])
			i++
		}
	}

	return "", 0, &SyntaxError{
		Pos:     i,
		Message: "unterminated quoted string",
		Hint:    `every " needs a closing "`,
	}
}

// lexRegex reads a /slash-delimited/ pattern.
func lexRegex(input string, i int) (string, int, error) {
	var sb strings.Builder
	i++ // opening slash

	for i < len(input) {
		switch input[i] {
		case '\\':
			if i+1 >= len(input) {
				return "", 0, &SyntaxError{Pos: i, Message: "trailing backslash inside a regex"}
			}
			// Escapes pass through untouched: the regex engine, not the lexer,
			// decides what \d means. Only \/ is consumed here, since that is
			// the delimiter.
			if input[i+1] == '/' {
				sb.WriteByte('/')
			} else {
				sb.WriteByte('\\')
				sb.WriteByte(input[i+1])
			}
			i += 2
		case '/':
			return sb.String(), i + 1, nil
		default:
			sb.WriteByte(input[i])
			i++
		}
	}

	return "", 0, &SyntaxError{
		Pos:     i,
		Message: "unterminated regex",
		Hint:    "a regex is delimited by slashes, e.g. message~/^GET /api/",
	}
}

// lexWord reads a bare token, stopping at whitespace or structural punctuation.
func lexWord(input string, i int) (string, int) {
	start := i

	for i < len(input) {
		r, size := utf8.DecodeRuneInString(input[i:])
		if unicode.IsSpace(r) || r == ':' || r == ',' || r == '"' {
			break
		}
		// A tilde after the first character ends the word, so that
		// message~timeout splits into key, operator, and value. Leading tildes
		// stay part of the word, since a value may legitimately begin with one.
		if r == '~' && i > start {
			break
		}
		i += size
	}

	return input[start:i], i
}

// SyntaxError is a parse failure with a position and, wherever possible, a
// copyable correct example.
//
// docs/FILTER-DSL.md section 7 is explicit that an error message containing a
// working example is worth more than any amount of documentation, so Hint is
// filled in whenever there is something concrete to suggest.
type SyntaxError struct {
	Pos     int
	Message string
	Hint    string
	Input   string
}

func (e *SyntaxError) Error() string {
	var sb strings.Builder
	sb.WriteString(e.Message)

	if e.Input != "" {
		fmt.Fprintf(&sb, "\n  %s\n  %s^", e.Input, strings.Repeat(" ", clampPos(e.Pos, e.Input)))
	}
	if e.Hint != "" {
		fmt.Fprintf(&sb, "\n%s", e.Hint)
	}
	return sb.String()
}

func clampPos(pos int, input string) int {
	if pos < 0 {
		return 0
	}
	if pos > len(input) {
		return len(input)
	}
	return pos
}
