package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VIGIL-OPS/loupe/internal/source"
)

// logDir writes a small directory of JSON lines and returns its path.
func logDir(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()

	if len(lines) == 0 {
		lines = []string{
			`{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"a","status":200}`,
			`{"ts":"2026-08-13T14:00:01Z","level":"error","msg":"b","status":500}`,
			`not json at all`,
		}
	}
	writeFile(t, dir, "app.log", strings.Join(lines, "\n")+"\n")
	return dir
}

func walk(t *testing.T, dir string) []source.Source {
	t.Helper()
	sources, err := source.Walk(dir, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return sources
}

// openCached is the common path: walk, open through the cache, register
// cleanup.
func openCached(t *testing.T, dir, cacheDir string, opts CacheOptions) *Cached {
	t.Helper()
	opts.Dir = cacheDir

	cached, err := OpenCached(context.Background(), walk(t, dir), LoadOptions{}, opts)
	if err != nil {
		t.Fatalf("OpenCached: %v", err)
	}
	t.Cleanup(func() { cached.DB.Close() })
	return cached
}

func TestFingerprintIsStable(t *testing.T) {
	sources := walk(t, logDir(t))

	first, ok := Fingerprint(sources, LoadOptions{})
	if !ok {
		t.Fatal("a directory of files should be cacheable")
	}

	for i := 0; i < 10; i++ {
		again, _ := Fingerprint(sources, LoadOptions{})
		if again != first {
			t.Fatalf("fingerprint is not stable: %q then %q", first, again)
		}
	}
}

// Everything that changes what lands in the table must change the fingerprint.
// A key that ignores any of these serves stale or wrong data.
func TestFingerprintIsSensitive(t *testing.T) {
	dir := logDir(t)
	base, _ := Fingerprint(walk(t, dir), LoadOptions{})

	t.Run("file contents", func(t *testing.T) {
		writeFile(t, dir, "app.log", `{"ts":"2026-08-13T14:00:00Z","msg":"different"}`+"\n")
		if got, _ := Fingerprint(walk(t, dir), LoadOptions{}); got == base {
			t.Error("fingerprint unchanged after the file changed")
		}
	})

	t.Run("parser override", func(t *testing.T) {
		fresh := logDir(t)
		a, _ := Fingerprint(walk(t, fresh), LoadOptions{})
		b, _ := Fingerprint(walk(t, fresh), LoadOptions{Parser: "text"})
		if a == b {
			t.Error("--parser does not change the fingerprint")
		}
	})

	// --source-tz moves timestamps, so a key without it would serve records
	// hours out.
	t.Run("source timezone", func(t *testing.T) {
		fresh := logDir(t)
		tokyo, err := time.LoadLocation("Asia/Tokyo")
		if err != nil {
			t.Skipf("tzdata unavailable: %v", err)
		}

		a, _ := Fingerprint(walk(t, fresh), LoadOptions{})
		b, _ := Fingerprint(walk(t, fresh), LoadOptions{
			SourceZones: map[string]*time.Location{"": tokyo},
		})
		if a == b {
			t.Error("--source-tz does not change the fingerprint; cached records would be hours out")
		}
	})

	// Map iteration order is random, so an unsorted hash of the zone overrides
	// would miss the cache at random.
	t.Run("zone map order does not matter", func(t *testing.T) {
		fresh := walk(t, logDir(t))
		tokyo, err := time.LoadLocation("Asia/Tokyo")
		if err != nil {
			t.Skipf("tzdata unavailable: %v", err)
		}

		opts := LoadOptions{SourceZones: map[string]*time.Location{
			"a": tokyo, "b": time.UTC, "c": tokyo, "d": time.UTC,
		}}
		want, _ := Fingerprint(fresh, opts)
		for i := 0; i < 20; i++ {
			if got, _ := Fingerprint(fresh, opts); got != want {
				t.Fatal("fingerprint varies with map iteration order")
			}
		}
	})
}

// A stream cannot be re-read, so there is nothing to invalidate against.
func TestStdinIsUncacheable(t *testing.T) {
	sources := append(walk(t, logDir(t)), source.NewStdin())

	if _, ok := Fingerprint(sources, LoadOptions{}); ok {
		t.Error("a set containing stdin should not be cacheable")
	}
}

// The point of the cache. Ingest, close, delete the source file, reopen: if the
// data is still there, the files genuinely were not read.
func TestCacheHitSkipsIngestion(t *testing.T) {
	dir := logDir(t)
	cacheDir := t.TempDir()

	first := openCached(t, dir, cacheDir, CacheOptions{})
	if first.Hit {
		t.Fatal("the first run reported a cache hit")
	}
	before, err := first.DB.Count(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	first.DB.Close()

	// Capture the walk before deleting, since Walk needs the files to exist.
	sources := walk(t, dir)
	if err := os.Remove(filepath.Join(dir, "app.log")); err != nil {
		t.Fatalf("remove source: %v", err)
	}

	second, err := OpenCached(context.Background(), sources, LoadOptions{},
		CacheOptions{Dir: cacheDir})
	if err != nil {
		t.Fatalf("OpenCached: %v", err)
	}
	defer second.DB.Close()

	if !second.Hit {
		t.Fatal("the second run re-ingested instead of reusing the cache")
	}
	after, err := second.DB.Count(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if after != before {
		t.Errorf("cached run has %d records, the original had %d", after, before)
	}
}

// A cached run must keep reporting the same caveats. Getting quieter about
// unparsed records on the second run is exactly backwards.
func TestCacheRestoresTheLoadSummary(t *testing.T) {
	dir := logDir(t)
	cacheDir := t.TempDir()

	first := openCached(t, dir, cacheDir, CacheOptions{})
	first.DB.Close()

	second := openCached(t, dir, cacheDir, CacheOptions{})
	if !second.Hit {
		t.Fatal("expected a cache hit")
	}

	if second.Load.Stats.Records != first.Load.Stats.Records {
		t.Errorf("records = %d, want %d", second.Load.Stats.Records, first.Load.Stats.Records)
	}
	if second.Load.Stats.Unparsed != first.Load.Stats.Unparsed {
		t.Errorf("unparsed = %d, want %d; the cached run stopped reporting damaged lines",
			second.Load.Stats.Unparsed, first.Load.Stats.Unparsed)
	}
	if second.Load.Stats.Unparsed == 0 {
		t.Error("the fixture has a malformed line; the count should not be zero")
	}
	if len(second.Load.Results) != len(first.Load.Results) {
		t.Errorf("got %d per-source results, want %d",
			len(second.Load.Results), len(first.Load.Results))
	}
}

func TestNoCacheBypasses(t *testing.T) {
	dir := logDir(t)
	cacheDir := t.TempDir()

	cached := openCached(t, dir, cacheDir, CacheOptions{Disabled: true})
	if cached.Hit {
		t.Error("--no-cache reported a hit")
	}
	if cached.Path != "" {
		t.Error("--no-cache wrote a cache file")
	}
	if !strings.Contains(cached.Reason, "no-cache") {
		t.Errorf("Reason = %q, want it to name the flag", cached.Reason)
	}

	entries, err := ListCache(cacheDir)
	if err != nil {
		t.Fatalf("ListCache: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("--no-cache left %d entries behind", len(entries))
	}
}

// A corrupt cache file must never block the tool.
func TestCorruptCacheFallsBack(t *testing.T) {
	dir := logDir(t)
	cacheDir := t.TempDir()

	first := openCached(t, dir, cacheDir, CacheOptions{})
	path := first.Path
	first.DB.Close()

	if err := os.WriteFile(path, []byte("this is not a duckdb file"), 0o644); err != nil {
		t.Fatalf("corrupt the cache: %v", err)
	}

	cached := openCached(t, dir, cacheDir, CacheOptions{})
	if cached.Hit {
		t.Error("a corrupt file was reported as a hit")
	}

	n, err := cached.DB.Count(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n == 0 {
		t.Error("no records after falling back to a fresh ingest")
	}
}

// An interrupted run must not leave a half-built database that a later run
// would trust.
func TestPartialFileIsNotInstalled(t *testing.T) {
	dir := logDir(t)
	cacheDir := t.TempDir()

	cached := openCached(t, dir, cacheDir, CacheOptions{})
	cached.DB.Close()

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".partial") {
			t.Errorf("a partial file was left behind: %s", e.Name())
		}
	}
}

// A cache written by an older ingest version must be rejected, or a user
// silently keeps reading records produced by parsers that have since been
// fixed. This is the failure IngestVersion exists to prevent, so it is tested
// by actually ageing a real cache file rather than by inspecting the constant.
func TestStaleIngestVersionIsRejected(t *testing.T) {
	dir := logDir(t)
	cacheDir := t.TempDir()

	first := openCached(t, dir, cacheDir, CacheOptions{})
	path := first.Path
	first.DB.Close()

	// Rewrite the recorded version to something older, as a cache written by a
	// previous release would have.
	aged, err := Open(path)
	if err != nil {
		t.Fatalf("reopen cache: %v", err)
	}
	if err := aged.Exec(context.Background(),
		`UPDATE loupe_cache_meta SET ingest_version = ?`, int64(IngestVersion-1)); err != nil {
		t.Fatalf("age the cache: %v", err)
	}
	if err := aged.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	cached := openCached(t, dir, cacheDir, CacheOptions{})
	if cached.Hit {
		t.Fatal("a cache from an older ingest version was reused")
	}
	if !strings.Contains(cached.Reason, "cache") {
		t.Errorf("Reason = %q, want it to explain the rebuild", cached.Reason)
	}

	// And it must have rebuilt rather than come back empty.
	n, err := cached.DB.Count(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n == 0 {
		t.Error("no records after rejecting the stale cache")
	}
}

// Two directories with different contents must never share a key.
func TestFingerprintDistinguishesDirectories(t *testing.T) {
	a, _ := Fingerprint(walk(t, logDir(t)), LoadOptions{})
	b, _ := Fingerprint(walk(t, logDir(t, `{"ts":"2026-08-13T14:00:00Z","msg":"different"}`)), LoadOptions{})

	if a == b {
		t.Error("two different directories produced the same fingerprint")
	}
}

func TestListAndClearCache(t *testing.T) {
	cacheDir := t.TempDir()

	for i := 0; i < 3; i++ {
		dir := logDir(t, `{"ts":"2026-08-13T14:00:0`+string(rune('0'+i))+`Z","msg":"x"}`)
		cached := openCached(t, dir, cacheDir, CacheOptions{})
		cached.DB.Close()
	}

	entries, err := ListCache(cacheDir)
	if err != nil {
		t.Fatalf("ListCache: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}

	// Newest first, so the listing reads as a recency order.
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Modified.Before(entries[i].Modified) {
			t.Error("entries are not ordered newest first")
		}
	}

	removed, freed, err := ClearCache(cacheDir)
	if err != nil {
		t.Fatalf("ClearCache: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed %d, want 3", removed)
	}
	if freed == 0 {
		t.Error("freed 0 bytes clearing three databases")
	}

	after, _ := ListCache(cacheDir)
	if len(after) != 0 {
		t.Errorf("%d entries survived the clear", len(after))
	}
}

func TestListCacheOnMissingDirectory(t *testing.T) {
	entries, err := ListCache(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("a missing cache directory should not error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries from a missing directory", len(entries))
	}
}

// The cache must not grow without limit, and must never evict the entry the
// current run just produced.
func TestPruneCacheRespectsTheCapAndKeepsTheNewest(t *testing.T) {
	cacheDir := t.TempDir()

	var paths []string
	for i := 0; i < 3; i++ {
		dir := logDir(t, `{"ts":"2026-08-13T14:00:0`+string(rune('0'+i))+`Z","msg":"x"}`)
		cached := openCached(t, dir, cacheDir, CacheOptions{})
		paths = append(paths, cached.Path)
		cached.DB.Close()
		// Distinct mtimes so the eviction order is well defined.
		time.Sleep(10 * time.Millisecond)
	}

	entries, err := ListCache(cacheDir)
	if err != nil {
		t.Fatalf("ListCache: %v", err)
	}
	newest := entries[0].Path

	// A cap of one byte forces eviction of everything evictable.
	removed, freed, err := PruneCache(cacheDir, 1, newest)
	if err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
	if removed != len(paths)-1 {
		t.Errorf("removed %d, want %d", removed, len(paths)-1)
	}
	if freed == 0 {
		t.Error("freed 0 bytes")
	}

	after, _ := ListCache(cacheDir)
	if len(after) != 1 {
		t.Fatalf("%d entries survived, want only the kept one", len(after))
	}
	if after[0].Path != newest {
		t.Errorf("kept %s, want the newest %s", after[0].Path, newest)
	}
}

func TestPruneCacheLeavesASmallCacheAlone(t *testing.T) {
	cacheDir := t.TempDir()

	dir := logDir(t)
	cached := openCached(t, dir, cacheDir, CacheOptions{})
	cached.DB.Close()

	removed, _, err := PruneCache(cacheDir, DefaultCacheLimit, "")
	if err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed %d entries from a cache well under the cap", removed)
	}
}
