package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GrantPukka/loupe/internal/source"
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

// openCached is the common path: walk, open through the cache, stamp the
// result complete, register cleanup.
//
// The stamp is what session.Open does once the whole pipeline — inference
// included — has succeeded. Without it nothing here would ever hit the cache,
// which is the point: see TestUnstampedCacheIsNotReused.
func openCached(t *testing.T, dir, cacheDir string, opts CacheOptions) *Cached {
	t.Helper()

	cached := openCachedRaw(t, dir, cacheDir, opts)
	if !cached.Hit && cached.Path != "" {
		if err := cached.DB.MarkComplete(context.Background()); err != nil {
			t.Fatalf("MarkComplete: %v", err)
		}
	}
	return cached
}

// openCachedRaw stops short of stamping, for the tests that are about the stamp.
func openCachedRaw(t *testing.T, dir, cacheDir string, opts CacheOptions) *Cached {
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
	// Note: file *contents* are deliberately absent from the fingerprint. They
	// are tracked per file in loupe_cache_files so a growing file can be
	// appended to rather than re-read. TestFingerprintSurvivesGrowth pins that,
	// and the incremental tests below pin that the change is still noticed.

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

// appendTo adds lines to an existing file, the way a running service does.
func appendTo(t *testing.T, dir, name string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
}

// A growing file must keep mapping to the same cache entry. When size and mtime
// were in the fingerprint, every append produced a new cache file and a full
// re-read — the limitation this replaces.
func TestFingerprintSurvivesGrowth(t *testing.T) {
	dir := logDir(t)
	before, _ := Fingerprint(walk(t, dir), LoadOptions{})

	appendTo(t, dir, "app.log", `{"ts":"2026-08-13T14:00:02Z","level":"warn","msg":"c"}`)

	if after, _ := Fingerprint(walk(t, dir), LoadOptions{}); after != before {
		t.Errorf("fingerprint changed when the file grew:\n  before %s\n  after  %s", before, after)
	}
}

// The point of the whole exercise: reopening a directory whose files have grown
// reads only what was appended, and ends up with exactly the records a full
// re-read would have produced.
func TestReopenAppendsOnlyTheNewRecords(t *testing.T) {
	dir := logDir(t)
	cacheDir := t.TempDir()
	ctx := context.Background()

	first := openCached(t, dir, cacheDir, CacheOptions{})
	before, err := first.DB.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	first.DB.Close()

	appendTo(t, dir, "app.log",
		`{"ts":"2026-08-13T14:00:02Z","level":"warn","msg":"c"}`,
		`{"ts":"2026-08-13T14:00:03Z","level":"error","msg":"d","status":503}`)

	second := openCached(t, dir, cacheDir, CacheOptions{})
	after, err := second.DB.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}

	if want := before + 2; after != want {
		t.Errorf("after appending 2 records the store holds %d, want %d", after, want)
	}

	// And the reported totals must match, not just the row count: these are the
	// numbers the status line puts in front of the user.
	if got := second.Load.Stats.Records; got != after {
		t.Errorf("summary reports %d records, the table holds %d", got, after)
	}

	// The same directory read cold must agree exactly.
	fresh := openCached(t, dir, t.TempDir(), CacheOptions{})
	freshCount, err := fresh.DB.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if freshCount != after {
		t.Errorf("incremental read holds %d records, a cold read holds %d", after, freshCount)
	}
	if !fresh.Load.Stats.Equal(second.Load.Stats) {
		t.Errorf("incremental stats differ from a cold read\n  incremental: %+v\n  cold:        %+v",
			second.Load.Stats, fresh.Load.Stats)
	}
}

// A file truncated and rewritten must not keep serving the records it used to
// hold. Size alone cannot detect this, which is why the head is hashed.
func TestRewrittenFileIsReReadNotAppended(t *testing.T) {
	dir := logDir(t)
	cacheDir := t.TempDir()
	ctx := context.Background()

	first := openCached(t, dir, cacheDir, CacheOptions{})
	if _, err := first.DB.Count(ctx); err != nil {
		t.Fatalf("count: %v", err)
	}
	first.DB.Close()

	// Same length, entirely different content: a log rotated in place.
	writeFile(t, dir, "app.log",
		`{"ts":"2026-08-13T15:00:00Z","level":"info","msg":"z","status":201}`+"\n")

	second := openCached(t, dir, cacheDir, CacheOptions{})
	count, err := second.DB.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("rewritten file left %d records, want 1", count)
	}

	var msg string
	if err := second.DB.QueryRow(ctx, `SELECT message FROM logs LIMIT 1`).Scan(&msg); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(msg, "z") {
		t.Errorf("message = %q, want the rewritten content", msg)
	}
}

// A record still being written when the first read stopped must be completed on
// the next read, not duplicated and not left as an orphan.
func TestRecordSplitAcrossReadsIsNotDuplicated(t *testing.T) {
	dir := t.TempDir()
	cacheDir := t.TempDir()
	ctx := context.Background()

	writeFile(t, dir, "app.log",
		`{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"first"}`+"\n"+
			`{"ts":"2026-08-13T14:00:01Z","level":"error","msg":"second"}`+"\n")

	first := openCached(t, dir, cacheDir, CacheOptions{})
	before, err := first.DB.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	first.DB.Close()

	appendTo(t, dir, "app.log", `{"ts":"2026-08-13T14:00:02Z","level":"warn","msg":"third"}`)

	second := openCached(t, dir, cacheDir, CacheOptions{})
	after, err := second.DB.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if after != before+1 {
		t.Errorf("store holds %d records, want %d — the boundary record was duplicated or lost",
			after, before+1)
	}

	// Line numbers must stay unique and continuous within the file.
	var distinct, total int64
	if err := second.DB.QueryRow(ctx,
		`SELECT count(DISTINCT line_no), count(*) FROM logs`).Scan(&distinct, &total); err != nil {
		t.Fatalf("line numbers: %v", err)
	}
	if distinct != total {
		t.Errorf("%d records share only %d distinct line numbers", total, distinct)
	}
}

// After an incremental append, the *reported* counts must match the table. The
// summary is what the status line prints, and a stale one understates the data
// on screen — the same class of failure as getting quieter about unparsed
// records on a second run.
func TestSummaryMatchesTheTableAfterAnAppend(t *testing.T) {
	dir := logDir(t)
	cacheDir := t.TempDir()
	ctx := context.Background()

	openCached(t, dir, cacheDir, CacheOptions{}).DB.Close()

	appendTo(t, dir, "app.log",
		`{"ts":"2026-08-13T14:00:02Z","level":"warn","msg":"c"}`,
		`{"ts":"2026-08-13T14:00:03Z","level":"info","msg":"d"}`)

	// The run that appends.
	openCached(t, dir, cacheDir, CacheOptions{}).DB.Close()

	// The run after that reads the stored summary rather than the files, so it
	// is the one that exposes a summary written badly.
	third := openCached(t, dir, cacheDir, CacheOptions{})
	if !third.Hit {
		t.Fatal("nothing changed; expected a cache hit")
	}

	rows, err := third.DB.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if third.Load.Stats.Records != rows {
		t.Errorf("summary reports %d records, the table holds %d",
			third.Load.Stats.Records, rows)
	}

	var metaRows int64
	if err := third.DB.QueryRow(ctx, `SELECT count(*) FROM loupe_cache_meta`).Scan(&metaRows); err != nil {
		t.Fatalf("count metadata rows: %v", err)
	}
	if metaRows != 1 {
		t.Errorf("loupe_cache_meta holds %d rows, want exactly 1", metaRows)
	}
}

// A cache file whose ingest never finished must never be reused. Appending the
// records is not the whole ingest — schema inference runs afterwards — and a
// half-built database is indistinguishable from a good one by record count
// alone, which is how a run silently loses every promoted column.
func TestUnstampedCacheIsNotReused(t *testing.T) {
	dir := logDir(t)
	cacheDir := t.TempDir()

	first := openCachedRaw(t, dir, cacheDir, CacheOptions{})
	if first.Hit {
		t.Fatal("the first open cannot be a hit")
	}
	if first.Path == "" {
		t.Fatal("a cacheable directory should have produced a cache file")
	}

	// Reopening now hits, because Session stamps the file after promotion.
	if err := first.DB.MarkComplete(context.Background()); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
	first.DB.Close()

	second := openCachedRaw(t, dir, cacheDir, CacheOptions{})
	if !second.Hit {
		t.Fatalf("a completed cache should be reused, got miss: %s", second.Reason)
	}

	// Now put it back the way an interrupted run would have left it.
	if err := second.DB.Exec(context.Background(),
		`UPDATE loupe_cache_meta SET complete = false`); err != nil {
		t.Fatalf("unstamp: %v", err)
	}
	second.DB.Close()

	third := openCachedRaw(t, dir, cacheDir, CacheOptions{})
	if third.Hit {
		t.Fatal("an unstamped cache was reused — a partial ingest can reach the user")
	}
	if !strings.Contains(third.Reason, "did not finish") {
		t.Errorf("reason = %q, want it to say the previous ingest did not finish", third.Reason)
	}
}

// A fresh ingest must not stamp itself. Only the caller that has finished the
// whole pipeline knows the ingest is complete.
func TestFreshIngestIsNotStampedComplete(t *testing.T) {
	dir := logDir(t)
	cacheDir := t.TempDir()

	cached := openCachedRaw(t, dir, cacheDir, CacheOptions{})
	cached.DB.Close()

	// Nothing called MarkComplete, so the next open must re-read.
	again := openCachedRaw(t, dir, cacheDir, CacheOptions{})
	if again.Hit {
		t.Fatal("an ingest that was never stamped complete was reused")
	}
}
