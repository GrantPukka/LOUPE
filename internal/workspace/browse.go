package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Root is a directory the browser may start from.
type Root struct {
	Path  string `json:"path"`
	Label string `json:"label"`
}

// Entry is one item in a browsed directory.
type Entry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size,omitempty"`
	// LooksLikeLog marks a file worth opening, so a directory of source code
	// does not read as a directory of logs.
	LooksLikeLog bool `json:"looks_like_log,omitempty"`
	// Subscribed marks a directory already being watched.
	Subscribed bool `json:"subscribed,omitempty"`
	// LogCount is how many files in a directory look like logs, so a browser
	// can show which folders are worth entering.
	LogCount int `json:"log_count,omitempty"`
}

// Listing is one directory's contents.
type Listing struct {
	Path string `json:"path"`
	// Parent is empty when the path is at a root, which is what stops the UI
	// offering to navigate above one.
	Parent  string  `json:"parent,omitempty"`
	Entries []Entry `json:"entries"`
	Roots   []Root  `json:"roots"`
	// Subscribed marks the listed directory itself.
	Subscribed bool `json:"subscribed"`
	// LogCount is how many entries here look like logs.
	LogCount int `json:"log_count"`
}

// Roots returns the directories browsing may start from.
//
// Deliberately not the whole filesystem. The API has no authentication and
// listens on a port; confining it means that even if the Host check were
// defeated, an attacker reaches a bounded set of directories rather than every
// file the user can read.
func (w *Workspace) Roots() []Root {
	var roots []Root
	seen := map[string]bool{}

	add := func(path, label string) {
		clean, err := Canonical(path)
		if err != nil || seen[clean] {
			return
		}
		if info, err := os.Stat(clean); err != nil || !info.IsDir() {
			return
		}
		seen[clean] = true
		roots = append(roots, Root{Path: clean, Label: label})
	}

	if home, err := os.UserHomeDir(); err == nil {
		add(home, "Home")
	}
	if wd, err := os.Getwd(); err == nil {
		add(wd, "Working directory")
	}

	// The conventional log location, where it exists.
	//
	// /tmp is deliberately not a root. Logs do land there, but so does every
	// application's scratch file, and it is world-writable — too broad a thing
	// to expose by default. Anyone with logs in /tmp can subscribe to the
	// specific directory from the command line.
	add("/var/log", "System logs")

	// Anything already subscribed is reachable, including its parent, so an
	// unsubscribed sibling can be found again without widening the roots for
	// everyone.
	for _, s := range w.Subscriptions {
		add(s.Path, s.Name())
		add(filepath.Dir(s.Path), filepath.Base(filepath.Dir(s.Path)))
	}

	sort.Slice(roots, func(i, j int) bool { return roots[i].Label < roots[j].Label })
	return roots
}

// Browse lists a directory, refusing anything outside the roots.
func (w *Workspace) Browse(path string) (Listing, error) {
	roots := w.Roots()

	if strings.TrimSpace(path) == "" {
		// No path means the root list itself, so a fresh UI has somewhere to
		// start without guessing.
		return Listing{Roots: roots}, nil
	}

	clean, err := Canonical(path)
	if err != nil {
		return Listing{}, err
	}

	root, ok := containingRoot(clean, roots)
	if !ok {
		return Listing{}, fmt.Errorf(
			"%s is outside the directories loupe will browse; "+
				"subscribe to it from the command line with `loupe subscribe %s`", clean, clean)
	}

	info, err := os.Stat(clean)
	if err != nil {
		return Listing{}, fmt.Errorf("cannot read %s: %w", clean, err)
	}
	if !info.IsDir() {
		return Listing{}, fmt.Errorf("%s is a file, not a directory", clean)
	}

	entries, err := os.ReadDir(clean)
	if err != nil {
		return Listing{}, fmt.Errorf("read %s: %w", clean, err)
	}

	listing := Listing{Path: clean, Roots: roots}
	if clean != root.Path {
		listing.Parent = filepath.Dir(clean)
	}
	if sub, found := w.Find(clean); found && sub.Active {
		listing.Subscribed = true
	}

	for _, e := range entries {
		// Hidden files are skipped for the same reason the walker skips them:
		// a log directory's dotfiles are configuration, not logs.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}

		full := filepath.Join(clean, e.Name())
		entry := Entry{Name: e.Name(), Path: full, IsDir: e.IsDir()}

		if e.IsDir() {
			entry.LogCount = countLogs(full)
			if sub, found := w.Find(full); found && sub.Active {
				entry.Subscribed = true
			}
		} else if info, err := e.Info(); err == nil {
			entry.Size = info.Size()
			entry.LooksLikeLog = looksLikeLog(e.Name(), info.Size())
			if entry.LooksLikeLog {
				listing.LogCount++
			}
		}

		listing.Entries = append(listing.Entries, entry)
	}

	// Directories first, then log-looking files, then the rest — the order a
	// person hunting for logs wants to read.
	sort.SliceStable(listing.Entries, func(i, j int) bool {
		a, b := listing.Entries[i], listing.Entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		if !a.IsDir && a.LooksLikeLog != b.LooksLikeLog {
			return a.LooksLikeLog
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})

	return listing, nil
}

// containingRoot finds the root a path sits under.
func containingRoot(path string, roots []Root) (Root, bool) {
	var best Root
	var found bool

	for _, root := range roots {
		if path == root.Path || strings.HasPrefix(path, root.Path+string(filepath.Separator)) {
			// The deepest matching root wins, so Parent stops at the closest
			// boundary rather than the broadest one.
			if !found || len(root.Path) > len(best.Path) {
				best, found = root, true
			}
		}
	}
	return best, found
}

// logExtensions are the suffixes that usually mean a log.
var logExtensions = map[string]bool{
	".log": true, ".txt": true, ".out": true, ".err": true,
	".jsonl": true, ".ndjson": true, ".json": true, ".gz": true,
}

// looksLikeLog is a hint for the browser, not a decision.
//
// The walker decides what is actually readable when a directory is opened; this
// only sorts and marks entries so a folder of source code does not look like a
// folder of logs.
func looksLikeLog(name string, size int64) bool {
	if size == 0 {
		return false
	}

	lower := strings.ToLower(name)
	if logExtensions[filepath.Ext(lower)] {
		return true
	}

	// Rotated logs carry a number after the extension: access.log.1, and
	// syslog has no extension at all.
	for _, known := range []string{"log", "syslog", "messages", "dmesg"} {
		if strings.Contains(lower, known) {
			return true
		}
	}
	return false
}

// countLogs counts the log-looking files directly inside a directory.
//
// One level only, and capped: this runs for every entry in a listing, and
// walking a whole tree to decorate a row would make browsing a home directory
// unusable.
func countLogs(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}

	const cap = 500
	count := 0

	for i, e := range entries {
		if i >= cap {
			break
		}
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if looksLikeLog(e.Name(), info.Size()) {
			count++
		}
	}
	return count
}
