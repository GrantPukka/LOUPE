// Package source is where bytes come from.
//
// A Source is an openable stream of log data with a stable identity: a local
// file, a directory walk, stdin, or a compressed file decompressed on the fly.
// Sources know nothing about log formats — turning bytes into records is
// [github.com/GrantPukka/loupe/internal/parse]'s job.
//
// Fingerprint is the basis of the cache layer, so it must change whenever the
// underlying bytes could have changed.
package source
