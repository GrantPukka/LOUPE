package parse

import "strconv"

// putField stores a value under key, keeping any value already there.
//
// Repeated keys are ordinary in structured logging: a logfmt line can carry
// three `user_note=` pairs, a JSON object is permitted to repeat a member, and
// an application that builds its line by appending will do both. Go's map
// assignment and encoding/json both resolve that by last-write-wins, which is
// a silent drop — and CLAUDE.md's rule for the fields bag is that a key is
// never dropped, because a key nobody can see is a key nobody knows to
// distrust.
//
// The second and later values are suffixed `.2`, `.3`, and so on, in the order
// the line wrote them. That keeps `user_note` meaning the first value — which
// is what a filter written against a well-behaved line expects — while
// `user_note.2` makes the rest reachable, and `loupe fields` shows the suffix
// so the repetition is visible rather than inferred.
func putField(fields map[string]any, key string, value any) {
	if _, taken := fields[key]; !taken {
		fields[key] = value
		return
	}
	for n := 2; ; n++ {
		suffixed := key + "." + strconv.Itoa(n)
		if _, taken := fields[suffixed]; !taken {
			fields[suffixed] = value
			return
		}
	}
}
