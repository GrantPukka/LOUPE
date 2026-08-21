package source

import (
	"bytes"
	"encoding/json"
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
//
// The single-file compression formats are deliberately absent. logrotate names
// its archives .gz, .bz2, .xz and .zst, and skipping those meant a directory of
// rotated logs read only the live file — silently, which is the failure this
// project refuses. Archive *containers* stay on the list: a .zip or .tar holds
// many files and reading one as a byte stream produces nonsense.
var skipExts = map[string]bool{
	".duckdb": true, ".db": true, ".sqlite": true, ".sqlite3": true,
	".zip": true, ".tar": true, ".7z": true,
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
	err = filepath.WalkDir(resolveRoot(root), func(path string, d fs.DirEntry, err error) error {
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

		info, reason := entryInfo(path, d)
		if info == nil {
			opts.skip(path, reason)
			return nil
		}

		f, reason := consider(path, d, info, opts)
		if f == nil {
			opts.skip(path, reason)
			return nil
		}
		f.linked = d.Type()&fs.ModeSymlink != 0
		files = append(files, f)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}

	files = dedupe(files, opts)
	sortRotation(files)

	out := make([]Source, len(files))
	for i, f := range files {
		out[i] = f
	}
	return out, nil
}

// consider decides whether one file should be read, returning the reason when
// it should not.
func consider(path string, d fs.DirEntry, info fs.FileInfo, opts *WalkOptions) (*File, string) {
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

	if info.Size() == 0 {
		return nil, "empty file"
	}

	f, err := NewFile(path)
	if err != nil {
		return nil, err.Error()
	}

	// The ceiling applies to uncompressed files only; a compressed file's
	// on-disk size says little about how much data it holds.
	if !f.Compressed() && info.Size() > opts.maxSize() {
		return nil, fmt.Sprintf("larger than %s (use --max-file-size)", humanSize(opts.maxSize()))
	}

	if binary, err := looksBinary(path); err != nil {
		return nil, err.Error()
	} else if binary {
		return nil, "appears to be binary"
	}

	if doc, err := looksJSONDocument(path); err != nil {
		return nil, err.Error()
	} else if doc {
		return nil, "a JSON document, not JSON lines"
	}

	return f, ""
}

// looksJSONDocument reports whether a file is one pretty-printed JSON value
// rather than a stream of JSON-lines records.
//
// Directories people point loupe at are full of package.json, tsconfig.json,
// and metadata files. Reading those as logs produces dozens of junk records
// that crowd out the real ones. JSON lines put one complete object per line, so
// an opening brace followed by an unterminated first line is the distinguishing
// feature and it needs only the first two lines to spot.
func looksJSONDocument(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	buf := make([]byte, 4096)
	n, err := io.ReadFull(f, buf)
	if err != nil && n == 0 {
		return false, nil
	}
	buf = bytes.TrimLeft(buf[:n], " \t\r\n")

	if len(buf) == 0 || (buf[0] != '{' && buf[0] != '[') {
		return false, nil
	}

	first := buf
	if i := bytes.IndexByte(buf, '\n'); i >= 0 {
		first = buf[:i]
	} else if n == len(buf) {
		// No newline in the whole prefix, so this is one very long line, which
		// is a plausible single-line JSON record.
		return false, nil
	}

	// A complete JSON value on the first line means JSON lines.
	return !json.Valid(bytes.TrimSpace(first)), nil
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

	// A compressed log is binary but wanted, and File handles the
	// decompression transparently.
	if detectCodec(buf) != codecNone {
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
// Every compression suffix is stripped, not only .gz, so that access.log.2.zst
// belongs to the same rotation group as access.log and file:access.log finds
// it. A suffix left on would put the archive in a group of its own, where it
// would sort separately and answer a filter nobody typed.
//
// The group includes the directory, so two access.log files in different
// directories are not treated as rotations of each other.
func rotationOf(path string) (group, name string, index int) {
	name = TrimCompressionSuffix(filepath.Base(path))

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

// compressionSuffixes are the extensions a rotated archive carries. Stripping
// them is a naming convenience only; what a file actually is comes from its
// magic bytes.
var compressionSuffixes = []string{".gz", ".zst", ".bz2", ".xz"}

// TrimCompressionSuffix removes the archive extension from a file name.
//
// Exported because internal/store reduces a path to a logical source name by
// the same rule, and the two lists must not drift: when this one learned zstd
// and the store's had only gzip, access.log.2.zst became a source of its own
// and source:access silently stopped matching it.
func TrimCompressionSuffix(name string) string {
	for _, suffix := range compressionSuffixes {
		if trimmed, ok := strings.CutSuffix(name, suffix); ok {
			return trimmed
		}
	}
	return name
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

// resolveRoot returns the path to walk from.
//
// filepath.WalkDir lstats its root, so a directory reached through a symlink is
// reported to the callback as a symlink, skipped as "not a regular file", and
// never descended into — `loupe /var/log/mylogs` finds nothing when mylogs is a
// link. Resolving first fixes that.
//
// Only when the root actually is a symlink. filepath.EvalSymlinks also cleans
// and absolutises, so resolving unconditionally would turn every displayed path
// from demo/app.log into /home/…/demo/app.log for no reason at all.
func resolveRoot(root string) string {
	info, err := os.Lstat(root)
	if err != nil || info.Mode()&fs.ModeSymlink == 0 {
		return root
	}

	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return root
	}
	return resolved
}

// entryInfo reports what a walked entry actually is, following a symlink to see
// what is on the other end.
//
// A symlink to a regular file is a regular file for reading, which is the whole
// point: /var/log/containers is a directory of links into /var/log/pods and is
// what Kubernetes documentation tells people to point a log tool at. A symlink
// to anything else is not, and neither is a socket, a device or a FIFO.
func entryInfo(path string, d fs.DirEntry) (fs.FileInfo, string) {
	if d.Type().IsRegular() {
		info, err := d.Info()
		if err != nil {
			return nil, err.Error()
		}
		return info, ""
	}

	if d.Type()&fs.ModeSymlink == 0 {
		return nil, "not a regular file"
	}

	// os.Stat follows the link. A broken one is a note, not a failure: a
	// container that exited between the directory listing and this call leaves
	// exactly that behind, and it must not stop the walk.
	info, err := os.Stat(path)
	if err != nil {
		return nil, "broken symlink"
	}

	switch {
	case info.IsDir():
		// Not followed, because a directory link can point at its own ancestor
		// and there is no cheap way to know it does not. Naming it directly
		// works, which is what resolveRoot is for.
		return nil, "a symlink to a directory — name it directly to read it"
	case !info.Mode().IsRegular():
		return nil, "a symlink to something that is not a regular file"
	}
	return info, ""
}

// dedupe drops files that are the same bytes reached under another name.
//
// This is the part of following symlinks that needs care rather than the
// following itself. `loupe /var/log` walks both pods/ and containers/, and
// every pod log is in both — so without this every count doubles, silently,
// which is the exact failure this project refuses. Fingerprint cannot catch it:
// it is built from the path, and the two paths differ.
//
// The real file wins over a link to it, so the name that appears in the source
// column is the one the bytes actually live at, whatever order the walk found
// them in.
func dedupe(files []*File, opts *WalkOptions) []*File {
	kept := make(map[string]int, len(files))
	out := make([]*File, 0, len(files))

	for _, f := range files {
		real := realPath(f.path)

		at, seen := kept[real]
		if !seen {
			kept[real] = len(out)
			out = append(out, f)
			continue
		}

		// Same bytes twice. Keep the one that is not a link, and report the
		// other rather than dropping it quietly.
		loser := f
		if f.linked == out[at].linked {
			// Two links to one file, or — impossible — two real paths. Keep
			// the first, which the lexical walk order makes deterministic.
		} else if !f.linked {
			loser = out[at]
			out[at] = f
		}
		opts.skip(loser.path, duplicateReason)
	}

	return out
}

// duplicateReason is fixed text rather than a sentence naming the other path,
// so that a directory where hundreds of files are reachable twice collapses to
// one line instead of hundreds. What was read is in `loupe sources`.
const duplicateReason = "already read under another name"

// realPath resolves a path for comparison, falling back to the path itself.
//
// A file that cannot be resolved is treated as its own identity rather than
// merged with anything, which errs toward reading a record twice over dropping
// one — and reading twice is at least visible.
func realPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}
