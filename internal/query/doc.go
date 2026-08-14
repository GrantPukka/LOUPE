// Package query implements the filter DSL.
//
// The pipeline is lexer -> parser -> AST -> parameterised SQL. Never build SQL
// by string concatenation here, however small the change looks: the time
// handling cannot be done correctly by substitution, and string-building is how
// injection bugs and unfixable precedence problems arrive.
//
// docs/FILTER-DSL.md is the specification. Read it before changing anything in
// this package. The rules most easily got wrong:
//
//   - last:15m is relative to the newest record loaded, not wall clock.
//   - Bare times resolve against the data's date range, and the resolved date is
//     reported to the user. Never resolve silently.
//   - A time filter reports how many records it excluded for having no
//     timestamp. Silently dropping them is a bug, not an edge case.
//   - An unknown field name is an error with a spelling suggestion, never an
//     empty result set.
//
// Every term type needs a round-trip test: parse(render(ast)) == ast.
package query
