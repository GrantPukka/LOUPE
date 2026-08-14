// Package server exposes the query layer over HTTP for the web UI, and embeds
// the built frontend assets.
//
// The API is deliberately tiny and the UI must not need anything beyond it.
// Every endpoint calls the same query path as the CLI — a UI capability with no
// CLI equivalent is a bug.
//
// It binds to localhost and makes no outbound connections.
package server
