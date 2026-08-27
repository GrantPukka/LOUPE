package parse

// shapeKey reduces the head of a line to the coarse pattern of its characters:
// digits become '#', ASCII letters become 'a', and everything else is kept as
// written.
//
// It exists so per-line detection can be memoised. Scoring twelve parsers means
// twelve regex scans of every line, and on a 250,000-line file that is three
// million scans to answer a question with about twenty distinct answers —
// `Aug ## ##:##:## a` is a syslog line whether it is the first or the
// hundred-thousandth.
//
// Twenty-four bytes is enough to separate the formats that differ at all in
// their opening: a comma before the milliseconds is Log4j and a full stop is
// Postgres. It is deliberately not enough to separate every one of them, which
// is why the cache stores an *ordering* rather than an answer — see
// mixedParser.Parse. A shape two formats share costs one failed Parse, never a
// wrong record.
func shapeKey(line []byte) string {
	const head = 24

	n := len(line)
	if n > head {
		n = head
	}

	key := make([]byte, n)
	for i := 0; i < n; i++ {
		c := line[i]
		switch {
		case c >= '0' && c <= '9':
			key[i] = '#'
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			key[i] = 'a'
		default:
			key[i] = c
		}
	}
	return string(key)
}
