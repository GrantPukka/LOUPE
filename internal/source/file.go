package source

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
)

// File is a log file on local disk. Compressed content is decompressed
// transparently, so callers never see compressed bytes.
type File struct {
	path string
	size int64
	// mtime is nanoseconds since the epoch, part of the fingerprint.
	mtime int64
	// codec is detected from the magic bytes rather than the extension, since
	// rotated logs are not reliably named.
	codec codec
	// linked records that this path is a symlink, which is what decides who
	// wins when the same bytes are reachable under two names.
	linked bool
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

	c, err := codecOf(path)
	if err != nil {
		return nil, err
	}

	return &File{
		path:  path,
		size:  info.Size(),
		mtime: info.ModTime().UnixNano(),
		codec: c,
	}, nil
}

func (f *File) Name() string { return f.path }
func (f *File) Size() int64  { return f.size }

// Compressed reports whether the file is compressed, which the walker uses to
// exempt it from the size ceiling.
func (f *File) Compressed() bool { return f.codec != codecNone }

// Codec names the compression, for reporting. Empty when there is none.
func (f *File) Codec() string {
	if f.codec == codecNone {
		return ""
	}
	return f.codec.String()
}

func (f *File) Open(ctx context.Context) (io.ReadCloser, error) {
	file, err := os.Open(f.path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", f.path, err)
	}

	if f.codec == codecNone {
		return file, nil
	}
	return decompress(f.codec, file, file, f.path)
}

// Fingerprint covers path, size, and mtime. A file rewritten in place with
// identical size and mtime is indistinguishable, which is the same bet every
// build system makes.
func (f *File) Fingerprint() string {
	return f.path + ":" + strconv.FormatInt(f.size, 10) + ":" + strconv.FormatInt(f.mtime, 10)
}

// PeekBytes is how much of a stream can be inspected without consuming it.
//
// Enough for the two hundred lines format detection samples, at any line
// length a log realistically has, and bounded so that a stream of one
// enormous line cannot be buffered without limit.
const PeekBytes = 1 << 20

// Peekable is a source whose leading bytes can be read without being consumed.
//
// Format detection samples a source and then the ingest reads it again. A file
// can simply be opened twice; a stream cannot, and sampling one the same way
// silently eats the first two hundred lines — the records are gone before
// anything has had a chance to count them, which is the exact failure this
// project refuses. A source that can be peeked lets detection look without
// taking.
type Peekable interface {
	Peek(n int) ([]byte, error)
}

// Stdin reads log data from standard input, for `kubectl logs -f api | loupe`.
//
// It deliberately does not implement Tailable. A stream has no seekable
// position and no stable identity, so it can never be resumed or re-read, and
// the incremental machinery must never be handed one.
type Stdin struct {
	src io.Reader

	// The stream is wrapped exactly once, however many times it is opened.
	// Every Open returns the same reader at the same position, because there
	// is only one position to be at.
	once   sync.Once
	buf    *bufio.Reader
	closer io.Closer
	err    error
}

// NewStdin reads from the process's standard input.
func NewStdin() *Stdin { return NewStream(os.Stdin) }

// NewStream reads from an arbitrary reader, which is what the tests use.
func NewStream(r io.Reader) *Stdin { return &Stdin{src: r} }

func (s *Stdin) Name() string { return "stdin" }

// Size is unknown for a stream.
//
// Minus one rather than zero, so a caller dividing by it to show progress gets
// an obviously wrong answer rather than a plausible one. Nothing in the ingest
// path does divide by it: the walker's size ceiling and the resume planner are
// its only readers, and neither ever sees a stream.
func (s *Stdin) Size() int64 { return -1 }

// Fingerprint is empty: a stream is never cacheable, since the same bytes will
// not be there to re-read.
func (s *Stdin) Fingerprint() string { return "" }

// reader wraps the stream once, decompressing a piped archive transparently.
//
// Detection by content is the only option a pipe offers: there is no name to
// infer from, and `cat old.log.zst | loupe` should not need a flag to say what
// is already obvious. The magic bytes are peeked rather than read, so nothing
// is taken from a stream that cannot give it back.
func (s *Stdin) reader() (*bufio.Reader, error) {
	s.once.Do(func() {
		raw := bufio.NewReaderSize(s.src, PeekBytes)

		magic, err := raw.Peek(magicBytes)
		if err != nil && !errors.Is(err, io.EOF) {
			s.err = fmt.Errorf("read stdin: %w", err)
			return
		}

		// A stream shorter than a signature is empty or nearly so. Not
		// compressed, and not an error: reading nothing is a legitimate
		// outcome.
		c := detectCodec(magic)
		if c == codecNone {
			s.buf = raw
			return
		}

		// Nothing underneath to close: the process's standard input is not
		// ours to close, and NewStream's reader belongs to its caller.
		rc, err := decompress(c, raw, nil, "stream")
		if err != nil {
			s.err = err
			return
		}
		s.buf, s.closer = bufio.NewReaderSize(rc, PeekBytes), rc
	})

	return s.buf, s.err
}

// Open returns the stream. Every call returns the same reader.
//
// The returned closer is a no-op. A stream outlives any single Open — format
// detection opens it before the ingest does — and closing it in between would
// end the read before it started.
func (s *Stdin) Open(ctx context.Context) (io.ReadCloser, error) {
	r, err := s.reader()
	if err != nil {
		return nil, err
	}
	return io.NopCloser(r), nil
}

// Peek returns the leading bytes that have arrived, without consuming them.
//
// It waits for the first byte and then takes whatever came with it, rather
// than waiting for n. bufio.Peek blocks until it has exactly the number asked
// for, and on a live pipe that number never arrives: `kubectl logs -f` on a
// quiet pod would hang in format detection before reading a single record —
// the precise failure this whole item exists to remove.
//
// The cost is that a slow producer is detected from less text than a file
// would be. That is the only option a stream offers, and `--parser` is there
// for the format that needs more than its first lines to recognise.
func (s *Stdin) Peek(n int) ([]byte, error) {
	r, err := s.reader()
	if err != nil {
		return nil, err
	}

	// Blocks until there is something to look at, or the stream ends. An empty
	// stream is a normal outcome, not a failure.
	if _, err := r.Peek(1); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, err
	}

	available := r.Buffered()
	if available > n {
		available = n
	}
	return r.Peek(available)
}

// CloseStream releases the decompressor, if any. The underlying stdin is the
// process's and is not ours to close.
func (s *Stdin) CloseStream() error {
	if s.closer == nil {
		return nil
	}
	return s.closer.Close()
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
