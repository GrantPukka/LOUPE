// Package store wraps DuckDB.
//
// It owns the database lifecycle, the logs table schema, batched ingest via the
// Appender API, and the fingerprint cache under ~/.cache/loupe. Queries arrive
// as parameterised SQL and are passed through; store does not know the filter
// DSL exists.
//
// Ingest streams. Memory stays bounded regardless of input size, so no code
// here may accumulate a whole file.
//
// store must not import server.
package store
