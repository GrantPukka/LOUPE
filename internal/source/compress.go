package source

import (
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// codec is the compression a source's bytes are stored in.
//
// Detected from magic bytes, never from the extension. Rotated logs are named
// inconsistently — a `.log` that is really gzip is common, and logrotate's
// `.1.gz` and `.1.zst` sit in the same directory as files with no suffix at
// all — so the content is the only thing worth trusting.
type codec int

const (
	codecNone codec = iota
	codecGzip
	codecZstd
	codecBzip2
	codecXZ
)

// magicBytes is how much of a file's head is needed to recognise every codec.
// xz has the longest signature at six bytes.
const magicBytes = 6

// magics maps each codec's signature to the codec.
//
// zstd's is the frame magic. A skippable frame can legitimately come first in a
// zstd stream, but its magic differs only in the low nibble of the last byte
// and the decoder skips it itself, so recognising the ordinary frame is enough
// to hand the file to a decoder that copes with both.
var magics = []struct {
	prefix []byte
	codec  codec
}{
	{[]byte{0x1f, 0x8b}, codecGzip},
	{[]byte{0x28, 0xb5, 0x2f, 0xfd}, codecZstd},
	{[]byte{0xfd, '7', 'z', 'X', 'Z', 0x00}, codecXZ},
	{[]byte("BZh"), codecBzip2},
}

func (c codec) String() string {
	switch c {
	case codecGzip:
		return "gzip"
	case codecZstd:
		return "zstd"
	case codecBzip2:
		return "bzip2"
	case codecXZ:
		return "xz"
	default:
		return "none"
	}
}

// detectCodec recognises a compression format from a file's leading bytes.
//
// A short read is not an error: a file of three bytes is not compressed, and
// refusing to open it would turn a harmless empty-ish file into a failure.
func detectCodec(head []byte) codec {
	for _, m := range magics {
		if bytes.HasPrefix(head, m.prefix) {
			// bzip2's signature is BZh followed by the block-size digit. Without
			// that check, a plain text file beginning "BZh" — a hostname, a
			// hash — would be handed to a decompressor that then fails the
			// whole file.
			if m.codec == codecBzip2 && !validBzip2Level(head) {
				continue
			}
			return m.codec
		}
	}
	return codecNone
}

func validBzip2Level(head []byte) bool {
	return len(head) > 3 && head[3] >= '1' && head[3] <= '9'
}

// codecOf reads a file's head and reports its compression.
func codecOf(path string) (codec, error) {
	f, err := os.Open(path)
	if err != nil {
		return codecNone, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	head := make([]byte, magicBytes)
	n, err := io.ReadFull(f, head)
	if err != nil && n == 0 {
		// Empty, or unreadable in a way the caller will meet again on open.
		return codecNone, nil
	}
	return detectCodec(head[:n]), nil
}

// decompress wraps a reader in the decoder for its codec.
//
// The returned closer owns everything it was given: the decompressor where one
// needs closing, and whatever was underneath. The stdlib gzip reader does not
// own its source, so closing only the outer reader leaks the descriptor — the
// same trap exists for every codec here.
func decompress(c codec, r io.Reader, under io.Closer, name string) (io.ReadCloser, error) {
	closers := []io.Closer{}
	if under != nil {
		closers = append(closers, under)
	}

	// A decompressor that will not start leaves the file open behind it, so
	// the chain is closed before the error goes back. Its own close error is
	// discarded on purpose: the reason the open failed is the one worth
	// reporting.
	fail := func(err error) (io.ReadCloser, error) {
		_ = closeAll(closers)
		return nil, fmt.Errorf("open %s %s: %w", c, name, err)
	}

	switch c {
	case codecNone:
		if rc, ok := r.(io.ReadCloser); ok && under == nil {
			return rc, nil
		}
		return &multiCloser{Reader: r, closers: closers}, nil

	case codecGzip:
		zr, err := gzip.NewReader(r)
		if err != nil {
			return fail(err)
		}
		return &multiCloser{Reader: zr, closers: append([]io.Closer{zr}, closers...)}, nil

	case codecZstd:
		// Concurrency is pinned to one goroutine per file. The ingest already
		// reads sources in parallel, and a decoder that spawns a worker per
		// core per file turns a directory of two hundred rotated archives into
		// a thread explosion.
		zr, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return fail(err)
		}
		// IOReadCloser gives a Close that releases the decoder's buffers;
		// Decoder.Close itself returns nothing and cannot be an io.Closer.
		return &multiCloser{Reader: zr, closers: append([]io.Closer{zr.IOReadCloser()}, closers...)}, nil

	case codecBzip2:
		// The stdlib decompressor is a plain io.Reader with nothing to release.
		return &multiCloser{Reader: bzip2.NewReader(r), closers: closers}, nil

	case codecXZ:
		xr, err := xz.NewReader(r)
		if err != nil {
			return fail(err)
		}
		return &multiCloser{Reader: xr, closers: closers}, nil

	default:
		return fail(fmt.Errorf("unknown codec %d", c))
	}
}

// multiCloser reads from one reader and closes a chain beneath it.
type multiCloser struct {
	io.Reader
	closers []io.Closer
}

func (m *multiCloser) Close() error { return closeAll(m.closers) }

// closeAll closes every closer and returns the first error, so that a failure
// in the decompressor does not leave the file descriptor open behind it.
func closeAll(closers []io.Closer) error {
	var err error
	for _, c := range closers {
		if cerr := c.Close(); err == nil {
			err = cerr
		}
	}
	return err
}
