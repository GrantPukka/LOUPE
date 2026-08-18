package pattern

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Masks are the placeholders a template can contain. They are spelled with
// angle brackets so a template reads as a sentence with holes in it, and so
// that a mask can never be mistaken for text that was actually logged.
const (
	MaskNum  = "<num>"
	MaskStr  = "<str>"
	MaskUUID = "<uuid>"
	MaskIP   = "<ip>"
	MaskID   = "<id>"
	MaskTime = "<ts>"
)

// IDLength is how many hex characters of the digest name a template.
//
// Twelve, not eight. Eight is 32 bits, and at a few thousand templates the
// birthday odds of a collision are around one in ten thousand — small, but the
// failure is that two unrelated templates silently become one under
// pattern:<id>, and silently merging records is the thing this project refuses
// to do. Twelve makes it not worth thinking about and is still short enough to
// type from a terminal.
const IDLength = 12

// Pattern is one message shape.
type Pattern struct {
	// Text is the template, e.g. "user <num> timed out".
	Text string
	// ID names it stably across runs and machines, so --new-since has
	// something to compare and pattern:<id> has something to select.
	ID string
}

// Of derives the template for a message.
//
// Only the first line is templated. A Log4j record carries its stack trace in
// the message, and every stack trace differs somewhere, so templating the whole
// thing would give every exception a template of its own — which is the same as
// having no templates at all for exactly the records that matter most.
func Of(message string) Pattern {
	text := Template(firstLine(message))
	return Pattern{Text: text, ID: idOf(text)}
}

// Template masks the variable tokens of a single line.
//
// A line with nothing to mask is returned as it came in, without allocating.
// Roughly half of a real corpus is messages like "request completed" that
// contain no values at all, and this runs on every record at ingest.
func Template(line string) string {
	var b strings.Builder

	// written is how much of line has been dealt with. It stays zero until the
	// first mask, which is what lets the untouched case return the original.
	written := 0

	for i := 0; i < len(line); {
		if isSpace(line[i]) {
			i++
			continue
		}

		start := i
		var masked string
		var changed bool

		// Quoted strings are found before the line is split on whitespace,
		// because the interesting ones contain it: "bad token" is one value in
		// two whitespace-delimited pieces, and a tokeniser that never saw it
		// whole would leave the message's most variable part unmasked.
		if m, next, ok := maskQuotedAt(line, i); ok {
			masked, i, changed = m, next, true
		} else {
			for i < len(line) && !isSpace(line[i]) {
				i++
			}
			tok := line[start:i]
			masked = maskToken(tok)
			changed = masked != tok
		}

		if !changed {
			continue
		}
		if written == 0 {
			b.Grow(len(line) + len(MaskNum))
		}
		b.WriteString(line[written:start])
		b.WriteString(masked)
		written = i
	}

	if written == 0 {
		return line
	}
	b.WriteString(line[written:])
	return b.String()
}

// idOf names a template by its content, so the same shape gets the same id in
// every run, on every machine, without anything being stored between runs.
func idOf(text string) string {
	sum := sha256.Sum256([]byte(text))

	var buf [IDLength]byte
	hex.Encode(buf[:], sum[:IDLength/2])
	return string(buf[:])
}

// maskToken replaces one whitespace-delimited token if it looks like a value.
//
// Order matters: the more specific shapes are tried first, because a uuid is
// also a run of hex and an ip is also digits and dots.
func maskToken(tok string) string {
	// Reject plain words in one pass. Every rule below needs either a digit or
	// one of the few marks that join a value's parts, so a word can be turned
	// away without being tried against all ten — and most tokens in a log
	// message are words.
	if !couldBeValue(tok) {
		return tok
	}

	// Punctuation wrapping a value is part of the sentence, not the value:
	// "(42)" is the number 42 in brackets, and "$1" is a placeholder whose
	// shape is the dollar. Peeling it off and putting it back keeps the
	// template readable and stops "(42)" and "42" being different shapes.
	lead, core, trail := peel(tok)
	if core == "" {
		return tok
	}

	masked, ok := maskValue(core)
	if !ok {
		return tok
	}
	if masked == core {
		return tok
	}
	if lead == "" && trail == "" {
		return masked
	}
	return lead + masked + trail
}

// couldBeValue reports whether a token has any chance of matching a rule.
//
// The punctuation marks are the ones that hold a value together rather than
// wrap it: the dot in an address or a decimal, the colon in a clock or an IPv6
// address, the slash in a path, the equals in a pair, the dash in a uuid. A
// token with none of them and no digit cannot be a value under any rule here.
func couldBeValue(tok string) bool {
	for i := 0; i < len(tok); i++ {
		switch c := tok[i]; {
		case isDigit(c):
			return true
		case c == '.' || c == '/' || c == '=' || c == ':' || c == '-':
			return true
		}
	}
	return false
}

// maskValue masks a token that has had its surrounding punctuation removed.
func maskValue(core string) (string, bool) {
	switch {
	case isTimestamp(core):
		return MaskTime, true
	case isUUID(core):
		return MaskUUID, true
	case isIP(core):
		return MaskIP, true
	}

	// A path is masked segment by segment rather than as a whole. Collapsing
	// /api/orders/2291 to <path> would also collapse /api/cart and
	// /api/checkout into it, and which endpoint is failing is usually the
	// entire finding.
	if strings.Contains(core, "/") && len(core) > 1 {
		return maskPath(core), true
	}

	// key=value keeps the key, which names what varied, and masks the value.
	//
	// The value is held to a looser standard than a bare token: the key has
	// already said what it is, so masking a short one like u_1 loses nothing,
	// where masking a bare u_1 in prose might.
	if key, value, found := strings.Cut(core, "="); found && key != "" && value != "" {
		if masked, ok := maskValue(value); ok {
			return key + "=" + masked, true
		}
		if isShortID(value) {
			return key + "=" + MaskID, true
		}
		return core, true
	}

	if masked, ok := maskNumeric(core); ok {
		return masked, true
	}
	if isOpaqueID(core) {
		return MaskID, true
	}

	return core, false
}

// maskPath masks the variable segments of a path, leaving the fixed ones.
func maskPath(path string) string {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		if seg == "" {
			continue
		}
		lead, core, trail := peel(seg)
		if core == "" {
			continue
		}
		switch {
		case isUUID(core):
			segments[i] = lead + MaskUUID + trail
		case isTimestamp(core):
			segments[i] = lead + MaskTime + trail
		case isOpaqueID(core):
			segments[i] = lead + MaskID + trail
		default:
			if masked, ok := maskNumeric(core); ok {
				segments[i] = lead + masked + trail
			}
		}
	}
	return strings.Join(segments, "/")
}

// maskQuotedAt masks a quoted string starting at i, returning where it ended.
//
// An unterminated quote is left alone and handled as an ordinary token. A line
// truncated mid-string is exactly when that happens, and swallowing the rest of
// the line would merge a corrupt record with a healthy one.
func maskQuotedAt(line string, i int) (masked string, next int, ok bool) {
	q := line[i]
	if q != '"' && q != '\'' && q != '`' {
		return "", 0, false
	}

	end := strings.IndexByte(line[i+1:], q)
	if end < 0 {
		return "", 0, false
	}
	end += i + 1

	// An empty pair is already as collapsed as it gets, and masking it would
	// make "" and "anything" the same shape.
	if end == i+1 {
		return "", 0, false
	}
	return string(q) + MaskStr + string(q), end + 1, true
}

// maskNumeric masks a number, keeping any unit attached to it.
//
// The unit is kept because "took <num>ms" and "took <num>s" are different
// shapes and collapsing them would lose an order of magnitude.
func maskNumeric(core string) (string, bool) {
	i := 0
	if i < len(core) && (core[i] == '-' || core[i] == '+') {
		i++
	}

	digits := 0
	for i < len(core) {
		switch {
		case isDigit(core[i]):
			digits++
		case core[i] == '.' || core[i] == ',':
			// A separator only counts as part of the number when a digit
			// follows it, so a trailing full stop stays sentence punctuation.
			if i+1 >= len(core) || !isDigit(core[i+1]) {
				goto done
			}
		default:
			goto done
		}
		i++
	}

done:
	if digits == 0 {
		return "", false
	}

	unit := core[i:]
	// A unit is a short alphabetic suffix: ms, s, kb, %. Anything longer is a
	// word with a number stuck to it, which is an opaque id, not a measurement.
	if unit != "" && !isUnit(unit) {
		return "", false
	}
	return MaskNum + unit, true
}

// peel splits leading and trailing punctuation off a token.
func peel(tok string) (lead, core, trail string) {
	start, end := 0, len(tok)
	for start < end && isPeelable(tok[start]) {
		start++
	}
	for end > start && isPeelable(tok[end-1]) {
		end--
	}
	return tok[:start], tok[start:end], tok[end:]
}

// isPeelable reports punctuation that wraps a value rather than belonging to
// it. '-' and '+' are absent on purpose: they are part of a signed number, and
// '.' and ':' because they are part of decimals, addresses, and timestamps.
func isPeelable(c byte) bool {
	switch c {
	case '(', ')', '[', ']', '{', '}', '<', '>', '"', '\'', '`', ',', ';', '$', '#', '@', '!', '?', '|':
		return true
	}
	return false
}

// isOpaqueID reports a token that is plainly an identifier: long enough to be
// deliberate, and mixing letters and digits the way a generated id does.
//
// The digit requirement is what keeps this conservative. Without it, every
// long word in the language would be masked and every message would collapse
// into the same template.
func isOpaqueID(core string) bool {
	const minLength = 8

	if len(core) < minLength {
		return false
	}

	var digits, letters int
	for i := 0; i < len(core); i++ {
		c := core[i]
		switch {
		case isDigit(c):
			digits++
		case isLetter(c):
			letters++
		case c == '_' || c == '-':
		default:
			return false
		}
	}
	return digits > 0 && letters > 0
}

// isShortID reports a value-position token that carries a digit, for the
// key=value case where the key already names what the value is.
//
// Three characters minimum, so a version marker like v2 keeps its identity
// while a generated id like u_1 does not.
func isShortID(value string) bool {
	const minLength = 3

	if len(value) < minLength {
		return false
	}

	var digits int
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case isDigit(c):
			digits++
		case isLetter(c) || c == '_' || c == '-':
		default:
			return false
		}
	}
	return digits > 0
}

// isUUID reports the canonical 8-4-4-4-12 form.
func isUUID(core string) bool {
	const uuidLength = 36

	if len(core) != uuidLength {
		return false
	}
	for i := 0; i < len(core); i++ {
		switch i {
		case 8, 13, 18, 23:
			if core[i] != '-' {
				return false
			}
		default:
			if !isHex(core[i]) {
				return false
			}
		}
	}
	return true
}

// isIP reports an IPv4 address, with or without a port, or an IPv6 address.
func isIP(core string) bool {
	host := core
	if i := strings.LastIndex(core, ":"); i >= 0 && strings.Count(core, ":") == 1 {
		host = core[:i]
	}

	if isIPv4(host) {
		return true
	}
	return isIPv6(core)
}

// isIPv4 scans rather than splitting. This runs on every token of every
// record, and a slice allocated per token is the difference between a
// templater that is free at ingest and one that is not.
func isIPv4(host string) bool {
	const octets = 4

	parts, digits := 1, 0
	for i := 0; i < len(host); i++ {
		switch c := host[i]; {
		case isDigit(c):
			digits++
			if digits > 3 {
				return false
			}
		case c == '.':
			if digits == 0 {
				return false
			}
			parts++
			if parts > octets {
				return false
			}
			digits = 0
		default:
			return false
		}
	}
	return parts == octets && digits > 0
}

// isIPv6 is deliberately loose: enough colons to be an address, and nothing in
// it that an address could not contain. A stricter parser here would be more
// code than the ambiguity is worth.
func isIPv6(core string) bool {
	if strings.Count(core, ":") < 2 {
		return false
	}
	for i := 0; i < len(core); i++ {
		if c := core[i]; !isHex(c) && c != ':' {
			return false
		}
	}
	return true
}

// isTimestamp reports the shapes that appear inside a message body: an ISO-8601
// instant, or a bare date or clock time.
func isTimestamp(core string) bool {
	switch {
	case looksLikeDate(core):
		return true
	case looksLikeClock(core):
		return true
	}

	// The combined form, with either separator.
	for _, sep := range []string{"T", " "} {
		if date, clock, found := strings.Cut(core, sep); found {
			clock = strings.TrimSuffix(clock, "Z")
			if looksLikeDate(date) && looksLikeClock(clock) {
				return true
			}
		}
	}
	return false
}

// looksLikeDate matches 2026-08-13.
func looksLikeDate(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	return allDigits(s[0:4]) && allDigits(s[5:7]) && allDigits(s[8:10])
}

// looksLikeClock matches 14:00:00, with optional fractional seconds and offset.
func looksLikeClock(s string) bool {
	if i := strings.IndexByte(s, '+'); i > 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '.'); i > 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(s, "Z")

	// hh:mm or hh:mm:ss, scanned in place rather than split.
	groups, digits := 1, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case isDigit(c):
			digits++
			if digits > 2 {
				return false
			}
		case c == ':':
			if digits != 2 {
				return false
			}
			groups++
			if groups > 3 {
				return false
			}
			digits = 0
		default:
			return false
		}
	}
	return groups >= 2 && digits == 2
}

// isUnit reports a short alphabetic suffix on a number, or a percent sign.
func isUnit(s string) bool {
	const maxUnit = 3

	if s == "%" {
		return true
	}
	if len(s) > maxUnit {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isLetter(s[i]) {
			return false
		}
	}
	return true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimRight(s[:i], "\r")
	}
	return s
}

func isSpace(c byte) bool  { return c == ' ' || c == '\t' }
func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func isHex(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
