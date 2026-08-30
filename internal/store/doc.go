// Package store wraps DuckDB.
//
// It owns the database lifecycle, the logs table schema, batched ingest via the
// Appender API, and the fingerprint cache under ~/.cache/loupe. Queries arrive
// as parameterised SQL and are passed through; store does not know the filter
// DSL exists.
//
// Ingest streams, and no code here may accumulate a whole file. That is a rule
// about this package's own buffers, not a claim about the process: the table
// itself lives in DuckDB and grows with the data, so peak RSS scales with input
// (see CLAUDE.md). The rule is what keeps the *parser* side flat, which is what
// makes a file larger than memory ingestable at all.
//
// store must not import server.
package store
