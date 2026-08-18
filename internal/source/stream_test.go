package source

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"strings"
	"testing"
)

func readAll(t *testing.T, s *Stdin) string {
	t.Helper()

	rc, err := s.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()

	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(body)
}

// Peeking must not take. Format detection peeks before the ingest reads, and a
// peek that consumed would eat the first two hundred lines of every stream —
// records gone before anything counted them.
func TestStreamPeekDoesNotConsume(t *testing.T) {
	const body = "one\ntwo\nthree\n"
	s := NewStream(strings.NewReader(body))

	head, err := s.Peek(4)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if string(head) != "one\n" {
		t.Errorf("Peek(4) = %q, want %q", head, "one\n")
	}

	if got := readAll(t, s); got != body {
		t.Errorf("after peeking, the stream read %q, want the whole of %q", got, body)
	}
}

// Detection opens the source, then the ingest opens it again. A stream has one
// position, so both must see the same bytes from the start.
func TestStreamOpenTwiceReadsFromWhereItLeftOff(t *testing.T) {
	const body = "alpha\nbeta\ngamma\n"
	s := NewStream(strings.NewReader(body))

	first, err := s.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Detection closes what it opened. That must not end the stream.
	first.Close()

	if got := readAll(t, s); got != body {
		t.Errorf("second Open read %q, want %q", got, body)
	}
}

// `zcat old.log.gz | loupe` should not need a flag to say what the magic bytes
// already say.
func TestStreamDecompressesPipedGzip(t *testing.T) {
	const body = "compressed line one\ncompressed line two\n"

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(body)); err != nil {
		t.Fatalf("write gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	s := NewStream(bytes.NewReader(buf.Bytes()))

	// Detection peeks through the decompressor, or it would score the format
	// against compressed bytes and pick nothing.
	head, err := s.Peek(len(body))
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if !strings.HasPrefix(string(head), "compressed line one") {
		t.Errorf("peeked %q, want decompressed text", head)
	}

	if got := readAll(t, s); got != body {
		t.Errorf("read %q, want %q", got, body)
	}
}

// An empty pipe is a normal outcome, not an error. `kubectl logs` on a pod
// that has not logged yet is the ordinary case.
func TestStreamHandlesAnEmptyPipe(t *testing.T) {
	s := NewStream(strings.NewReader(""))

	head, err := s.Peek(64)
	if err != nil {
		t.Fatalf("Peek on an empty stream: %v", err)
	}
	if len(head) != 0 {
		t.Errorf("peeked %q from an empty stream", head)
	}
	if got := readAll(t, s); got != "" {
		t.Errorf("read %q from an empty stream", got)
	}
}

// A stream shorter than the peek window is the common case, and asking for
// more than there is must not be an error.
func TestStreamPeekBeyondTheEnd(t *testing.T) {
	s := NewStream(strings.NewReader("short\n"))

	head, err := s.Peek(PeekBytes)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if string(head) != "short\n" {
		t.Errorf("peeked %q, want the whole stream", head)
	}
}

// The incremental machinery must never be handed a stream: there is no offset
// to resume from and no identity to compare against.
func TestStreamIsNotTailable(t *testing.T) {
	var s Source = NewStdin()

	if _, ok := s.(Tailable); ok {
		t.Error("Stdin implements Tailable; a stream has no seekable position")
	}
}
