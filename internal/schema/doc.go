// Package schema turns records into columns.
//
// Fields appearing in most of a sampled prefix are promoted to real typed
// DuckDB columns; everything else stays in the fields JSON bag, still
// queryable. Nothing is ever dropped.
//
// This package also owns the table of timestamp layouts tried during
// inference, which is a common and welcome contribution.
package schema
