package source

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
)

// File is a log file on local disk. Gzip content is decompressed
// transparently, so callers never see compressed bytes.
type File struct {
	path string
	size int64
	// mtime is nanoseconds since the epoch, part of the fingerprint.
	mtime int64
	// gzipped is detected from the magic bytes rather than the extension, since
	// rotated logs are not reliably named.
	gzipped bool
}

// NewFile stats the path and detects compression.
func NewFile(path string) (*File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("stat %s: is a directory", path)
	}

	gzipped, err := isGzip(path)
	if err != nil {
		return nil, err
	}

	return &File{
		path:    path,
		size:    info.Size(),
		mtime:   info.ModTime().UnixNano(),
		gzipped: gzipped,
	}, nil
}

func (f *File) Name() string { return f.path }
func (f *File) Size() int64  { return f.size }

// Compressed reports whether the file is gzipped, which the walker uses to
// exempt it from the size ceiling.
func (f *File) Compressed() bool { return f.gzipped }

func (f *File) Open(ctx context.Context) (io.ReadCloser, error) {
	file, err := os.Open(f.path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", f.path, err)
	}

	if !f.gzipped {
		return file, nil
	}

	zr, err := gzip.NewReader(file)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("open gzip %s: %w", f.path, err)
	}
	return &gzipReadCloser{Reader: zr, underlying: file}, nil
}

// Fingerprint covers path, size, and mtime. A file rewritten in place with
// identical size and mtime is indistinguishable, which is the same bet every
// build system makes.
func (f *File) Fingerprint() string {
	return f.path + ":" + strconv.FormatInt(f.size, 10) + ":" + strconv.FormatInt(f.mtime, 10)
}

// gzipReadCloser closes the gzip reader and the file beneath it. The stdlib
// gzip reader does not own its source, so closing only the outer reader leaks
// the descriptor.
type gzipReadCloser struct {
	*gzip.Reader
	underlying io.Closer
}

func (g *gzipReadCloser) Close() error {
	err := g.Reader.Close()
	if cerr := g.underlying.Close(); err == nil {
		err = cerr
	}
	return err
}

// isGzip checks the magic bytes rather than trusting the extension. Rotated
// logs get named inconsistently and a .log that is really gzip is common.
func isGzip(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var magic [2]byte
	n, err := io.ReadFull(f, magic[:])
	if err != nil && n < 2 {
		// Shorter than two bytes, so empty or nearly so, and certainly not gzip.
		return false, nil
	}
	return magic[0] == 0x1f && magic[1] == 0x8b, nil
}

// Stdin reads log data from standard input, for `kubectl logs -f api | loupe`.
type Stdin struct{}

func NewStdin() *Stdin { return &Stdin{} }

func (s *Stdin) Name() string { return "stdin" }

// Size is unknown for a stream.
func (s *Stdin) Size() int64 { return -1 }

// Fingerprint is empty: a stream is never cacheable, since the same bytes will
// not be there to re-read.
func (s *Stdin) Fingerprint() string { return "" }

func (s *Stdin) Open(ctx context.Context) (io.ReadCloser, error) {
	// Stdin may be piped gzip, e.g. `zcat old.log.gz | loupe`, but peeking to
	// find out would consume bytes from a stream that cannot be rewound, so it
	// is deliberately not attempted.
	return io.NopCloser(os.Stdin), nil
}

// LogicalName strips the directory, compression suffix, and rotation number, so
// that /var/log/nginx/access.log.2.gz becomes access.log.
//
// Every file in a rotation group shares a logical name, which is what lets
// file:access.log match the rotated files too.
func (f *File) LogicalName() string {
	_, name, _ := rotationOf(f.path)
	return name
}

// RotationIndex is 0 for a live file, and higher for older rotated copies:
// access.log is 0, access.log.1 is 1, access.log.2.gz is 2.
func (f *File) RotationIndex() int {
	_, _, n := rotationOf(f.path)
	return n
}
