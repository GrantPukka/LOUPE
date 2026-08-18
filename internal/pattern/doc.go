// Package pattern collapses log messages into templates.
//
// The problem it solves is triage: thirty-four thousand lines are not thirty-four
// thousand different things, they are a dozen shapes with the variable parts
// filled in differently. "user 4821 timed out" and "user 9903 timed out" are one
// event with a count, and seeing them that way is what makes an anomaly visible
// in a wall of noise.
//
// The collapse rule is deliberately conservative and deliberately inspectable.
// Only value-shaped tokens are masked — numbers, uuids, addresses, quoted
// strings, hex ids, timestamps — and a bare word is never touched. Two messages
// that differ by a word stay two templates.
//
// That is the right way to be wrong. Splitting one event into two templates is a
// cosmetic annoyance; merging two genuinely different errors into one hides the
// incident, which is the failure this package exists to avoid. The template text
// shows exactly what was collapsed, so the rule can always be checked by eye.
package pattern
