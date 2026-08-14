package source

import (
	"context"
	"io"
)

// Source is an openable stream of log bytes with a stable identity.
//
// Implementations know nothing about log formats. Turning bytes into records is
// the parse package's job.
type Source interface {
	// Name is the display name, usually a path. It is what appears in the
	// source column and in filter terms like file:access.log.
	Name() string

	// Open returns the bytes. The caller closes the reader. Compression is
	// handled transparently, so callers always see plain text.
	Open(ctx context.Context) (io.ReadCloser, error)

	// Size is the number of bytes, or -1 when unknown, as for streams. It is
	// used for progress reporting and for the directory walker's size ceiling,
	// so an inaccurate value is cosmetic rather than fatal.
	Size() int64

	// Fingerprint identifies the content for cache invalidation. It must change
	// whenever the underlying bytes could have changed. Streams return an empty
	// string, meaning uncacheable.
	Fingerprint() string
}
