// Package render writes results out.
//
// Table for a TTY, JSON and NDJSON for pipes, raw for grep compatibility, and
// the handoff writers described in docs/HANDOFF.md.
//
// Truncated output always says it was truncated and states the full count. An
// extract that hides its own incompleteness is worse than no extract.
package render
