package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// HeadBytes is the most of a file's start that is hashed to decide whether it
// is still the same file.
//
// Enough to be distinctive, small enough that hashing it on every open costs
// nothing. A log file rewritten in place or replaced by rotation changes within
// its first lines in practice; one that does not is indistinguishable from a
// file that merely grew, which is the same bet Fingerprint already makes.
const HeadBytes = 4096

// Tailable is a source that can be re-opened partway through.
//
// It is an optional interface rather than part of Source, for the same reason
// Continuer is not part of Parser: a stream cannot implement it. stdin has no
// seekable position and no stable identity, so it is never incrementally
// re-ingested and must not be forced to pretend otherwise.
type Tailable interface {
	// OpenAt returns the bytes from offset onward. The caller closes it.
	OpenAt(ctx context.Context, offset int64) (io.ReadCloser, error)

	// Head hashes the first n bytes of the file. A changed hash means the file
	// was rewritten or replaced rather than appended to, and anything
	// previously read from it is no longer trustworthy.
	//
	// The length is the caller's choice because the comparison must cover the
	// same byte range both times. A file shorter than the window would
	// otherwise hash its own new content on every append, and every append
	// would look like a rewrite.
	Head(n int64) (string, error)
}

// OpenAt seeks to offset and reads from there.
//
// A compressed file cannot be resumed partway through, since the offset refers
// to decompressed bytes that only exist by decompressing everything before
// them. That is not a limitation worth engineering around: a rotated archive
// does not grow, whatever it is compressed with, so the incremental path never
// asks.
func (f *File) OpenAt(ctx context.Context, offset int64) (io.ReadCloser, error) {
	if offset == 0 {
		return f.Open(ctx)
	}
	if f.codec != codecNone {
		return nil, fmt.Errorf("open %s at %d: %s files cannot be resumed", f.path, offset, f.codec)
	}

	file, err := os.Open(f.path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", f.path, err)
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		file.Close()
		return nil, fmt.Errorf("seek %s to %d: %w", f.path, offset, err)
	}
	return file, nil
}

// Head hashes the first n bytes of the file on disk.
//
// It reads the raw bytes, compressed or not: the question is whether this is
// still the same file, and the compressed bytes answer it just as well. A short
// read is not an error — a file that has shrunk below n is itself evidence of a
// truncation, and hashing what remains reports a different digest, which is the
// answer the caller wants.
func (f *File) Head(n int64) (string, error) {
	if n <= 0 || n > HeadBytes {
		n = HeadBytes
	}

	file, err := os.Open(f.path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", f.path, err)
	}
	defer file.Close()

	buf := make([]byte, n)
	read, err := io.ReadFull(file, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", fmt.Errorf("read head of %s: %w", f.path, err)
	}

	sum := sha256.Sum256(buf[:read])
	return hex.EncodeToString(sum[:])[:32], nil
}
