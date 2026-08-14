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
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func writeGzip(t *testing.T, dir, name, content string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := io.WriteString(zw, content); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func read(t *testing.T, s Source) string {
	t.Helper()
	rc, err := s.Open(context.Background())
	if err != nil {
		t.Fatalf("open %s: %v", s.Name(), err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", s.Name(), err)
	}
	return string(b)
}

func names(sources []Source) []string {
	out := make([]string, len(sources))
	for i, s := range sources {
		out[i] = filepath.Base(s.Name())
	}
	return out
}

func TestFileReadsPlainText(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "app.log", "line one\nline two\n")

	f, err := NewFile(path)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	if got := read(t, f); got != "line one\nline two\n" {
		t.Errorf("content = %q", got)
	}
	if f.Size() != 18 {
		t.Errorf("Size() = %d, want 18", f.Size())
	}
	if f.Compressed() {
		t.Error("plain file reported as compressed")
	}
}

// Compression is detected from magic bytes, not the extension, because rotated
// logs are named inconsistently and a gzipped file called .log is common.
func TestGzipIsTransparentAndDetectedByContent(t *testing.T) {
	dir := t.TempDir()
	const content = "compressed line\n"

	tests := []struct {
		name string
		file string
	}{
		{"conventional extension", "old.log.gz"},
		{"gzip content with a misleading .log name", "misnamed.log"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeGzip(t, dir, tt.file, content)
			f, err := NewFile(path)
			if err != nil {
				t.Fatalf("NewFile: %v", err)
			}
			if !f.Compressed() {
				t.Fatal("gzip content not detected")
			}
			if got := read(t, f); got != content {
				t.Errorf("content = %q, want %q", got, content)
			}
		})
	}
}

// A .gz extension on content that is not gzip must not cause a decompression
// error; the file should be read as text.
func TestMisnamedGzipExtensionIsReadAsText(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "notreally.gz", "plain text\n")

	f, err := NewFile(path)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	if f.Compressed() {
		t.Error("plain content behind a .gz name reported as compressed")
	}
	if got := read(t, f); got != "plain text\n" {
		t.Errorf("content = %q", got)
	}
}

func TestFingerprintChangesWithContent(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "app.log", "one\n")

	first, err := NewFile(path)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	before := first.Fingerprint()

	if before == "" {
		t.Fatal("fingerprint is empty")
	}

	// Same bytes, re-statted: the cache must consider this unchanged.
	again, err := NewFile(path)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	if again.Fingerprint() != before {
		t.Error("fingerprint changed without the file changing; the cache would never hit")
	}

	write(t, dir, "app.log", "one\ntwo\n")
	grown, err := NewFile(path)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	if grown.Fingerprint() == before {
		t.Error("fingerprint unchanged after the file grew; stale cache would be served")
	}
}

// A stream cannot be re-read, so it must never claim to be cacheable.
func TestStdinIsNotCacheable(t *testing.T) {
	s := NewStdin()
	if s.Fingerprint() != "" {
		t.Errorf("Fingerprint() = %q, want empty", s.Fingerprint())
	}
	if s.Size() != -1 {
		t.Errorf("Size() = %d, want -1 for an unknown-length stream", s.Size())
	}
}

func TestWalkAcceptsASingleFile(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "app.log", "hello\n")

	got, err := Walk(path, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != 1 || got[0].Name() != path {
		t.Fatalf("got %v, want the single file", names(got))
	}
}

func TestWalkFindsFilesRecursively(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "app.log", "a\n")
	write(t, dir, "nested/deep/other.log", "b\n")

	got, err := Walk(dir, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 files", names(got))
	}
}

// Rotated files must read oldest first, so that a streaming ingest of records
// without timestamps still ends up in a sensible order.
func TestWalkOrdersRotationGroupsOldestFirst(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "access.log", "newest\n")
	write(t, dir, "access.log.1", "middle\n")
	writeGzip(t, dir, "access.log.2.gz", "oldest\n")

	got, err := Walk(dir, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	want := []string{"access.log.2.gz", "access.log.1", "access.log"}
	if diff := strings.Join(names(got), ","); diff != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", names(got), want)
	}
}

// Two files with the same base name in different directories are not rotations
// of one another.
func TestWalkDoesNotGroupAcrossDirectories(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a/access.log", "one\n")
	write(t, dir, "b/access.log.1", "two\n")

	got, err := Walk(dir, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want both files", names(got))
	}
	if !strings.Contains(got[0].Name(), filepath.Join("a", "access.log")) {
		t.Errorf("first = %s, want the file under a/", got[0].Name())
	}
}

func TestWalkSkipsWhatIsNotALog(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "app.log", "keep me\n")
	write(t, dir, "empty.log", "")
	write(t, dir, ".hidden.log", "hidden\n")
	write(t, dir, "image.png", "not really a png but the extension is enough\n")
	write(t, dir, ".git/objects/abc", "git internals\n")
	write(t, dir, "node_modules/pkg/out.log", "dependency noise\n")
	write(t, dir, "binary.log", "text\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00")

	opts := &WalkOptions{}
	got, err := Walk(dir, opts)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if len(got) != 1 || filepath.Base(got[0].Name()) != "app.log" {
		t.Fatalf("got %v, want only app.log", names(got))
	}

	// Every skip must be reported. A user whose logs were all skipped needs to
	// be told why, not shown an empty table.
	if len(opts.Skipped) == 0 {
		t.Error("nothing recorded in Skipped; skips would be invisible to the user")
	}
	for _, s := range opts.Skipped {
		if s.Reason == "" {
			t.Errorf("%s skipped with no reason given", s.Path)
		}
	}
}

// The blaster writes NUL bytes into individual damaged lines on purpose, so a
// few must not condemn the whole file as binary.
func TestWalkKeepsTextFilesContainingAFewNULs(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "damaged.log", strings.Repeat("a normal log line here\n", 100)+"broken\x00\x00line\n")

	got, err := Walk(dir, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %v, want the damaged file kept", names(got))
	}
}

func TestWalkIncludeAndExclude(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "access.log", "a\n")
	write(t, dir, "error.log", "b\n")
	write(t, dir, "debug.txt", "c\n")

	tests := []struct {
		name string
		opts WalkOptions
		want []string
	}{
		{"no filters", WalkOptions{}, []string{"access.log", "debug.txt", "error.log"}},
		{"include glob", WalkOptions{Include: []string{"*.log"}}, []string{"access.log", "error.log"}},
		{"exclude glob", WalkOptions{Exclude: []string{"debug.*"}}, []string{"access.log", "error.log"}},
		{"exclude wins over include", WalkOptions{
			Include: []string{"*.log"}, Exclude: []string{"error.log"},
		}, []string{"access.log"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Walk(dir, &tt.opts)
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			if strings.Join(names(got), ",") != strings.Join(tt.want, ",") {
				t.Errorf("got %v, want %v", names(got), tt.want)
			}
		})
	}
}

func TestWalkSizeCeiling(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "big.log", strings.Repeat("x", 5000)+"\n")
	write(t, dir, "small.log", "tiny\n")

	opts := &WalkOptions{MaxFileSize: 1000}
	got, err := Walk(dir, opts)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != 1 || filepath.Base(got[0].Name()) != "small.log" {
		t.Fatalf("got %v, want only small.log", names(got))
	}

	var found bool
	for _, s := range opts.Skipped {
		if strings.Contains(s.Path, "big.log") && strings.Contains(s.Reason, "--max-file-size") {
			found = true
		}
	}
	if !found {
		t.Error("oversized file skipped without naming the flag that would include it")
	}
}

// A compressed file's on-disk size says nothing about its content length, so
// the ceiling must not apply to it.
func TestWalkSizeCeilingExemptsCompressedFiles(t *testing.T) {
	dir := t.TempDir()
	writeGzip(t, dir, "archive.log.gz", strings.Repeat("compressible\n", 1000))

	got, err := Walk(dir, &WalkOptions{MaxFileSize: 1})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %v, want the compressed file kept", names(got))
	}
}

func TestLogicalNameAndRotationIndex(t *testing.T) {
	tests := []struct {
		path      string
		wantName  string
		wantIndex int
	}{
		{"access.log", "access.log", 0},
		{"access.log.1", "access.log", 1},
		{"access.log.2.gz", "access.log", 2},
		{"/var/log/nginx/access.log.14.gz", "access.log", 14},
		{"syslog", "syslog", 0},
		{"app.2026-08-13.log", "app.2026-08-13.log", 0},
		{"checkout-api.log", "checkout-api.log", 0},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			_, name, index := rotationOf(tt.path)
			if name != tt.wantName {
				t.Errorf("name = %q, want %q", name, tt.wantName)
			}
			if index != tt.wantIndex {
				t.Errorf("index = %d, want %d", index, tt.wantIndex)
			}
		})
	}
}

// An unreadable subdirectory must not abort the walk, or one bad permission
// hides every other log file.
func TestWalkContinuesPastUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, permissions are not enforced")
	}
	dir := t.TempDir()
	write(t, dir, "readable.log", "visible\n")
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	opts := &WalkOptions{}
	got, err := Walk(dir, opts)
	if err != nil {
		t.Fatalf("Walk aborted on an unreadable directory: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %v, want the readable file", names(got))
	}
	if len(opts.Skipped) == 0 {
		t.Error("unreadable directory not reported to the user")
	}
}

func TestWalkMissingPathErrors(t *testing.T) {
	if _, err := Walk(filepath.Join(t.TempDir(), "nope"), nil); err == nil {
		t.Fatal("expected an error for a missing path")
	}
}
