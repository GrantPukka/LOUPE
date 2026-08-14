// Package parse turns bytes into records.
//
// This is the project's extension point: adding a log format means one new
// file, one init() registration, and one golden-file fixture. Nothing else in
// the codebase changes. The Parser interface is deliberately tiny and stays
// that way — do not add a method to accommodate a single awkward format.
//
// The governing rule is that a malformed line never aborts a file. A parser
// returns ErrNoMatch for a line it cannot handle and the pipeline keeps the raw
// text, marked unparsed. A record with no extractable timestamp is still a
// record.
//
// parse must not import store, query, or server.
package parse
