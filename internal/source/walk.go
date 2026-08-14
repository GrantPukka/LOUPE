package source

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// DefaultMaxFileSize is the ceiling above which an uncompressed file is skipped
// during a directory walk. Someone pointing loupe at a directory containing a
// 40GB archive wants the other files read, not a hang.
//
// Compressed files are exempt: their decompressed size is unknown and their
// on-disk size is not comparable.
const DefaultMaxFileSize = 4 << 30 // 4GiB

// skipDirs are never descended into. Walking a .git directory produces
// thousands of binary objects and no logs.
var skipDirs = map[string]bool{
	".git":         true,
	".svn":         true,
	".hg":          true,
	"node_modules": true,
	".terraform":   true,
	"__pycache__":  true,
	".venv":        true,
	"vendor":       true,
}

// skipExts are extensions that are never log files. This is a courtesy filter
// for speed; the binary content check below is what actually guarantees
// correctness.
var skipExts = map[string]bool{
	".duckdb": true, ".db": true, ".sqlite": true, ".sqlite3": true,
	".zip": true, ".tar": true, ".bz2": true, ".xz": true, ".zst": true, ".7z": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".pdf": true, ".ico": true,
	".so": true, ".dylib": true, ".dll": true, ".exe": true, ".a": true, ".o": true,
	".class": true, ".jar": true, ".pyc": true, ".wasm": true,
	".mp4": true, ".mov": true, ".mp3": true, ".woff": true, ".woff2": true, ".ttf": true,
}

// WalkOptions tunes a directory walk. The zero value is the intended default
// for `loupe ./logs`, which must work on a messy directory with no flags.
type WalkOptions struct {
	// MaxFileSize overrides DefaultMaxFileSize when non-zero.
	MaxFileSize int64

	// Include, when non-empty, keeps only files whose base name matches one of
	// these globs.
	Include []string

	// Exclude drops files whose base name matches any of these globs. It is
	// applied after Include.
	Exclude []string

	// Skipped collects a note for every file passed over, so the caller can
	// tell the user what was not read. Silence about skipped files is how a
	// user concludes their logs contain nothing.
	Skipped []Skip
}

// Skip records one file that the walk did not return, and why.
type Skip struct {
	Path   string
	Reason string
}

func (o *WalkOptions) maxSize() int64 {
	if o.MaxFileSize > 0 {
		return o.MaxFileSize
	}
	return DefaultMaxFileSize
}

func (o *WalkOptions) skip(path, reason string) {
	o.Skipped = append(o.Skipped, Skip{Path: path, Reason: reason})
}

// Walk returns every readable log file under root, ordered chronologically
// within each rotation group.
//
// A single file path is also accepted, so callers do not need to stat first.
func Walk(root string, opts *WalkOptions) ([]Source, error) {
	if opts == nil {
		opts = &WalkOptions{}
	}

	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", root, err)
	}
	if !info.IsDir() {
		f, err := NewFile(root)
		if err != nil {
			return nil, err
		}
		return []Source{f}, nil
	}

	var files []*File
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is worth reporting but must not abort the
			// walk: one permission-denied subdirectory should not hide the rest.
			opts.skip(path, err.Error())
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			if path != root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}

		if !d.Type().IsRegular() {
			opts.skip(path, "not a regular file")
			return nil
		}

		f, reason := consider(path, d, opts)
		if f == nil {
			opts.skip(path, reason)
			return nil
		}
		files = append(files, f)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}

	sortRotation(files)

	out := make([]Source, len(files))
	for i, f := range files {
		out[i] = f
	}
	return out, nil
}

// consider decides whether one file should be read, returning the reason when
// it should not.
func consider(path string, d fs.DirEntry, opts *WalkOptions) (*File, string) {
	name := d.Name()

	if strings.HasPrefix(name, ".") {
		return nil, "hidden file"
	}
	if skipExts[strings.ToLower(filepath.Ext(name))] {
		return nil, "extension is not a log format"
	}
	if !matches(name, opts.Include, true) {
		return nil, "did not match --include"
	}
	if matches(name, opts.Exclude, false) {
		return nil, "matched --exclude"
	}

	info, err := d.Info()
	if err != nil {
		return nil, err.Error()
	}
	if info.Size() == 0 {
		return nil, "empty file"
	}

	f, err := NewFile(path)
	if err != nil {
		return nil, err.Error()
	}

	// The ceiling applies to uncompressed files only; a compressed file's
	// on-disk size says little about how much data it holds.
	if !f.gzipped && info.Size() > opts.maxSize() {
		return nil, fmt.Sprintf("larger than %s (use --max-file-size)", humanSize(opts.maxSize()))
	}

	if binary, err := looksBinary(path); err != nil {
		return nil, err.Error()
	} else if binary {
		return nil, "appears to be binary"
	}

	return f, ""
}

// matches reports whether name matches any pattern. An empty pattern list
// returns whenEmpty, which differs between include and exclude semantics.
func matches(name string, patterns []string, whenEmpty bool) bool {
	if len(patterns) == 0 {
		return whenEmpty
	}
	for _, p := range patterns {
		if ok, err := filepath.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}

// looksBinary reads a prefix and looks for NUL bytes, the same heuristic grep
// uses. Note that the blaster deliberately writes NUL bytes into individual
// damaged lines; a handful in a text file is normal, so the test is on the
// proportion rather than presence.
func looksBinary(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	buf := make([]byte, 8000)
	n, err := io.ReadFull(f, buf)
	if err != nil && n == 0 {
		if err == io.EOF {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	buf = buf[:n]

	// Gzip is binary but wanted, and File handles the decompression.
	if n >= 2 && buf[0] == 0x1f && buf[1] == 0x8b {
		return false, nil
	}

	var nuls int
	for _, b := range buf {
		if b == 0 {
			nuls++
		}
	}
	return nuls*100 > n, nil
}

// rotationOf splits a path into its rotation group, its logical file name, and
// its rotation number. access.log is 0, access.log.1 is 1, access.log.2.gz is
// 2. Higher numbers are older, which is what makes chronological ordering
// possible without opening the files.
//
// The group includes the directory, so two access.log files in different
// directories are not treated as rotations of each other.
func rotationOf(path string) (group, name string, index int) {
	name = strings.TrimSuffix(filepath.Base(path), ".gz")

	if i := strings.LastIndex(name, "."); i > 0 {
		if n, err := strconv.Atoi(name[i+1:]); err == nil {
			name, index = name[:i], n
		}
	}
	return filepath.Join(filepath.Dir(path), name), name, index
}

// sortRotation orders files so that a rotation group reads oldest first, and
// groups themselves are ordered by name for a stable result.
//
// Ordering here is a convenience for streaming ingest and for the unsorted
// display case. Records carrying timestamps get sorted properly by the store.
func sortRotation(files []*File) {
	sort.SliceStable(files, func(i, j int) bool {
		gi, _, ni := rotationOf(files[i].path)
		gj, _, nj := rotationOf(files[j].path)
		if gi != gj {
			return gi < gj
		}
		return ni > nj // higher rotation number is older, so it comes first
	})
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.0f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
