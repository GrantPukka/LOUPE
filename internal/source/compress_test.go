package source

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// bzip2Sample is a real bzip2 stream of "alpha\nbeta\ngamma\n", produced by
// bzip2 -9 and checked in as bytes.
//
// The standard library decompresses bzip2 and cannot produce it, and adding a
// compressor as a test dependency to test a decompressor would be the tail
// wagging the dog. A fixture the test cannot generate is exactly what a golden
// byte string is for.
var bzip2Sample = []byte{
	0x42, 0x5a, 0x68, 0x39, 0x31, 0x41, 0x59, 0x26, 0x53, 0x59, 0x45, 0xdd,
	0xc7, 0x7a, 0x00, 0x00, 0x03, 0x41, 0x80, 0x00, 0x10, 0x32, 0xc6, 0x44,
	0x00, 0x20, 0x00, 0x22, 0x1a, 0x0c, 0x9a, 0x10, 0x03, 0x01, 0x28, 0xbc,
	0x40, 0x86, 0x90, 0x6f, 0xc5, 0xdc, 0x91, 0x4e, 0x14, 0x24, 0x11, 0x77,
	0x71, 0xde, 0x80,
}

// compressors produce each format from the same plain text, so one table can
// drive every codec.
var compressors = map[string]struct {
	codec codec
	// encode returns the compressed bytes, or nil when the format is only
	// available as a checked-in sample.
	encode func(t *testing.T, plain []byte) []byte
	sample []byte
	// plain is what sample decompresses to, for the checked-in case.
	plain string
}{
	"gzip": {codec: codecGzip, encode: func(t *testing.T, plain []byte) []byte {
		t.Helper()
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(plain); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
		return buf.Bytes()
	}},
	"zstd": {codec: codecZstd, encode: func(t *testing.T, plain []byte) []byte {
		t.Helper()
		var buf bytes.Buffer
		w, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatalf("zstd writer: %v", err)
		}
		if _, err := w.Write(plain); err != nil {
			t.Fatalf("zstd write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("zstd close: %v", err)
		}
		return buf.Bytes()
	}},
	"xz": {codec: codecXZ, encode: func(t *testing.T, plain []byte) []byte {
		t.Helper()
		var buf bytes.Buffer
		w, err := xz.NewWriter(&buf)
		if err != nil {
			t.Fatalf("xz writer: %v", err)
		}
		if _, err := w.Write(plain); err != nil {
			t.Fatalf("xz write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("xz close: %v", err)
		}
		return buf.Bytes()
	}},
	"bzip2": {codec: codecBzip2, sample: bzip2Sample, plain: "alpha\nbeta\ngamma\n"},
}

// The headline: every format reads back as the text that went in, and callers
// never see a compressed byte.
func TestFileReadsEveryCodec(t *testing.T) {
	for name, c := range compressors {
		t.Run(name, func(t *testing.T) {
			plain := "alpha\nbeta\ngamma\n"
			body := c.sample
			if body == nil {
				body = c.encode(t, []byte(plain))
			} else {
				plain = c.plain
			}

			path := filepath.Join(t.TempDir(), "app.log."+name)
			if err := os.WriteFile(path, body, 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}

			f, err := NewFile(path)
			if err != nil {
				t.Fatalf("NewFile: %v", err)
			}
			if f.codec != c.codec {
				t.Errorf("detected %s, want %s", f.codec, c.codec)
			}
			if !f.Compressed() {
				t.Error("Compressed() = false")
			}
			if f.Codec() != name {
				t.Errorf("Codec() = %q, want %q", f.Codec(), name)
			}

			rc, err := f.Open(context.Background())
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if err := rc.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
			if string(got) != plain {
				t.Errorf("read %q, want %q", got, plain)
			}
		})
	}
}

// Detection is by content, never by name: a rotated log called .log that is
// really zstd must still read, and a .gz that is really plain text must not be
// handed to a decompressor.
func TestCodecIsDetectedFromContentNotName(t *testing.T) {
	dir := t.TempDir()

	misnamed := filepath.Join(dir, "access.log")
	if err := os.WriteFile(misnamed, compressors["zstd"].encode(t, []byte("compressed\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := NewFile(misnamed)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	if f.codec != codecZstd {
		t.Errorf("a zstd file named .log detected as %s", f.codec)
	}

	lying := filepath.Join(dir, "old.log.gz")
	if err := os.WriteFile(lying, []byte("plain text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err = NewFile(lying)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	if f.codec != codecNone {
		t.Errorf("a plain file named .gz detected as %s", f.codec)
	}
}

// bzip2's signature is three letters and a digit. Text beginning "BZh" is not
// rare — a hostname, a hash — and handing it to a decompressor would fail the
// whole file over four bytes.
func TestBzip2NeedsItsLevelDigit(t *testing.T) {
	tests := map[string]codec{
		"BZh9":     codecBzip2,
		"BZh1":     codecBzip2,
		"BZh0":     codecNone,
		"BZhX":     codecNone,
		"BZhost-1": codecNone,
		"BZ":       codecNone,
	}

	for head, want := range tests {
		if got := detectCodec([]byte(head)); got != want {
			t.Errorf("detectCodec(%q) = %s, want %s", head, got, want)
		}
	}
}

func TestDetectCodecOnShortInput(t *testing.T) {
	for _, head := range []string{"", "x", "\x1f", "\x28\xb5"} {
		if got := detectCodec([]byte(head)); got != codecNone {
			t.Errorf("detectCodec(%q) = %s, want none", head, got)
		}
	}
}

// A compressed source is never Tailable. EC001 assumes it, and an offset into
// decompressed bytes cannot be seeked to without decompressing everything
// before it.
func TestCompressedFilesCannotBeResumed(t *testing.T) {
	for name, c := range compressors {
		t.Run(name, func(t *testing.T) {
			body := c.sample
			if body == nil {
				body = c.encode(t, []byte("one\ntwo\n"))
			}

			path := filepath.Join(t.TempDir(), "app.log")
			if err := os.WriteFile(path, body, 0o644); err != nil {
				t.Fatal(err)
			}
			f, err := NewFile(path)
			if err != nil {
				t.Fatalf("NewFile: %v", err)
			}

			if _, err := f.OpenAt(context.Background(), 4); err == nil {
				t.Fatal("a compressed file was resumed partway through")
			} else if !strings.Contains(err.Error(), "cannot be resumed") {
				t.Errorf("error does not say why: %v", err)
			}

			// From the start it is an ordinary open, which is what a re-read
			// asks for.
			rc, err := f.OpenAt(context.Background(), 0)
			if err != nil {
				t.Fatalf("OpenAt(0): %v", err)
			}
			rc.Close()
		})
	}
}

// A piped archive is decompressed transparently, because a pipe has no name to
// infer from and `zstdcat old.log.zst | loupe` should not need a flag.
func TestStreamReadsEveryCodec(t *testing.T) {
	for name, c := range compressors {
		t.Run(name, func(t *testing.T) {
			plain := "alpha\nbeta\n"
			body := c.sample
			if body == nil {
				body = c.encode(t, []byte(plain))
			} else {
				plain = c.plain
			}

			s := NewStream(bytes.NewReader(body))
			rc, err := s.Open(context.Background())
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != plain {
				t.Errorf("read %q, want %q", got, plain)
			}
			if err := s.CloseStream(); err != nil {
				t.Errorf("CloseStream: %v", err)
			}
		})
	}
}

// Peeking a compressed stream shows decompressed text, or format detection
// would be shown compressed bytes and pick the fallback parser every time.
func TestStreamPeeksDecompressed(t *testing.T) {
	body := compressors["zstd"].encode(t, []byte(`{"level":"info","msg":"hello"}`+"\n"))

	s := NewStream(bytes.NewReader(body))
	got, err := s.Peek(PeekBytes)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if !bytes.HasPrefix(got, []byte(`{"level"`)) {
		t.Errorf("peeked %q, want decompressed JSON", got)
	}
}

// A rotated archive belongs to the same rotation group as the live file it came
// from, whatever it is compressed with — otherwise file:access.log finds only
// some of the rotation and says nothing about the rest.
func TestRotationGroupsIgnoreCompressionSuffix(t *testing.T) {
	tests := []struct {
		path  string
		name  string
		index int
	}{
		{"/var/log/access.log", "access.log", 0},
		{"/var/log/access.log.1", "access.log", 1},
		{"/var/log/access.log.2.gz", "access.log", 2},
		{"/var/log/access.log.3.zst", "access.log", 3},
		{"/var/log/access.log.4.bz2", "access.log", 4},
		{"/var/log/access.log.5.xz", "access.log", 5},
	}

	for _, tc := range tests {
		group, name, index := rotationOf(tc.path)
		if name != tc.name || index != tc.index {
			t.Errorf("rotationOf(%q) = %q/%d, want %q/%d", tc.path, name, index, tc.name, tc.index)
		}
		if want := "/var/log/access.log"; group != want {
			t.Errorf("rotationOf(%q) grouped as %q, want %q", tc.path, group, want)
		}
	}
}

// The walk must return compressed archives, not skip them by extension. Doing
// so meant a directory of rotated logs read only the live file, silently.
func TestWalkReadsCompressedArchives(t *testing.T) {
	dir := t.TempDir()

	write := func(name string, body []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("access.log", []byte("live\n"))
	write("access.log.1.gz", compressors["gzip"].encode(t, []byte("one\n")))
	write("access.log.2.zst", compressors["zstd"].encode(t, []byte("two\n")))
	write("access.log.3.bz2", bzip2Sample)
	write("access.log.4.xz", compressors["xz"].encode(t, []byte("four\n")))

	var opts WalkOptions
	got, err := Walk(dir, &opts)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if len(got) != 5 {
		names := make([]string, len(got))
		for i, s := range got {
			names[i] = filepath.Base(s.Name())
		}
		t.Fatalf("walked %d file(s), want 5: %v (skipped: %+v)", len(got), names, opts.Skipped)
	}

	// Oldest first within the rotation group, so a streaming ingest reads them
	// in the order they were written.
	want := []string{"access.log.4.xz", "access.log.3.bz2", "access.log.2.zst", "access.log.1.gz", "access.log"}
	for i, s := range got {
		if base := filepath.Base(s.Name()); base != want[i] {
			t.Errorf("position %d is %s, want %s", i, base, want[i])
		}
	}
}

// A compressed file is exempt from the size ceiling: its on-disk size says
// nothing about how much log it holds.
func TestCompressedFilesAreExemptFromTheSizeCeiling(t *testing.T) {
	dir := t.TempDir()
	body := compressors["zstd"].encode(t, bytes.Repeat([]byte("a log line\n"), 1000))
	if err := os.WriteFile(filepath.Join(dir, "big.log.zst"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	opts := WalkOptions{MaxFileSize: 1}
	got, err := Walk(dir, &opts)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("walked %d file(s), want 1 — a compressed file was measured by its on-disk size (%+v)",
			len(got), opts.Skipped)
	}
}
