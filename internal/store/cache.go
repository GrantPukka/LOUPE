package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GrantPukka/loupe/internal/source"
)

// IngestVersion is bumped whenever a change would make a cached database
// disagree with a fresh ingest of the same files.
//
// Any change to the table schema, to a parser's output, to level
// normalisation, or to timestamp handling belongs here. Forgetting to bump it
// means users silently keep reading data produced by the old code, which is a
// nastier bug than a slow re-ingest.
const IngestVersion = 8

// cacheMetaTable holds one row describing how the cached database was built.
const cacheMetaTable = `
CREATE TABLE IF NOT EXISTS loupe_cache_meta (
    fingerprint     VARCHAR NOT NULL,
    ingest_version  BIGINT  NOT NULL,
    created_at      TIMESTAMP NOT NULL,
    summary         VARCHAR NOT NULL,  -- JSON, see cachedSummary
    complete        BOOLEAN NOT NULL   -- see MarkComplete
)`

// CacheOptions controls the on-disk cache.
type CacheOptions struct {
	// Dir overrides the cache location. Empty means ~/.cache/loupe.
	Dir string

	// Disabled bypasses the cache entirely, for --no-cache.
	Disabled bool
}

// cachedSummary is what a cache hit restores so the status line can report the
// same counts and assumptions a cold run would have.
//
// Without this, a cached run would silently stop mentioning unparsed records
// and assumed timezones — the tool would get quieter about its own caveats the
// second time you ran it, which is precisely backwards.
type cachedSummary struct {
	Results []IngestResult `json:"results"`
	Stats   parseStatsJSON `json:"stats"`
	Took    time.Duration  `json:"took"`
}

// parseStatsJSON mirrors parse.Stats for storage. It exists so the cache format
// does not silently change shape when that struct gains a field.
type parseStatsJSON struct {
	Lines        int64 `json:"lines"`
	Records      int64 `json:"records"`
	Unparsed     int64 `json:"unparsed"`
	NoTimestamp  int64 `json:"no_timestamp"`
	Continuation int64 `json:"continuation"`
	Truncated    int64 `json:"truncated"`
	Blank        int64 `json:"blank"`
	ZoneAssumed  int64 `json:"zone_assumed"`
	InvalidUTF8  int64 `json:"invalid_utf8"`
}

// Fingerprint identifies a set of sources, not their contents.
//
// It covers the ingest version, the options that affect parsing, and each
// source's path — deliberately not its size or mtime. Content changes are
// tracked per file in loupe_cache_files instead, so a directory being written
// to keeps mapping to the same cache file and can be appended to. With size and
// mtime still here, every growing file produced a fresh fingerprint, a fresh
// cache file and a full re-read, which is the limitation ARCHITECTURE.md 3.4
// describes.
//
// --source-tz is included because it moves timestamps, and a cache keyed
// without it would serve records an hour out.
//
// It returns false when any source is uncacheable, which is the case for
// stdin: a stream cannot be re-read, so there is nothing to invalidate against.
func Fingerprint(sources []source.Source, opts LoadOptions) (string, bool) {
	h := sha256.New()

	fmt.Fprintf(h, "v%d\n", IngestVersion)
	fmt.Fprintf(h, "parser=%s\n", opts.Parser)

	// Map iteration order is random, so the zone overrides are sorted before
	// hashing. An unstable fingerprint would miss the cache every time.
	zones := make([]string, 0, len(opts.SourceZones))
	for name, loc := range opts.SourceZones {
		if loc != nil {
			zones = append(zones, name+"="+loc.String())
		}
	}
	sort.Strings(zones)
	fmt.Fprintf(h, "zones=%s\n", strings.Join(zones, ","))

	// Sources are hashed in walk order, which is already deterministic. An empty
	// source fingerprint still means uncacheable, but only its identity is
	// hashed here — the bytes are the file-state table's business.
	for _, s := range sources {
		if s.Fingerprint() == "" {
			return "", false
		}
		fmt.Fprintf(h, "src=%s\n", s.Name())
	}

	return hex.EncodeToString(h.Sum(nil))[:32], true
}

// CacheDir resolves where cached databases live.
func CacheDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}

	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate cache directory: %w", err)
	}
	return filepath.Join(base, "loupe"), nil
}

// Cached is the outcome of opening a set of sources, from cache or freshly
// ingested.
type Cached struct {
	DB   *DB
	Load Load

	// Hit is true when ingestion was skipped.
	Hit bool

	// Path is the cache file backing this database, empty when uncached.
	Path string

	// Reason explains why the cache was not used, for the status line. A user
	// wondering why the second run was not instant deserves an answer.
	Reason string
}

// OpenCached opens the sources, reusing a cached database when one matches.
//
// This is what makes a re-open feel instant, and ARCHITECTURE.md 3.4 rates it a
// bigger perceived-quality win than any amount of query optimisation.
func OpenCached(ctx context.Context, sources []source.Source, load LoadOptions, cache CacheOptions) (*Cached, error) {
	if cache.Disabled {
		return ingestFresh(ctx, sources, load, "", "--no-cache")
	}

	fingerprint, ok := Fingerprint(sources, load)
	if !ok {
		return ingestFresh(ctx, sources, load, "", "a source is a stream and cannot be cached")
	}

	dir, err := CacheDir(cache.Dir)
	if err != nil {
		return ingestFresh(ctx, sources, load, "", err.Error())
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ingestFresh(ctx, sources, load, "", fmt.Sprintf("cannot create %s: %v", dir, err))
	}

	path := filepath.Join(dir, fingerprint+".duckdb")

	if hit, err := openHit(ctx, path, fingerprint); err == nil && hit != nil {
		hit.Path = path

		// A cache hit is not the end of the question any more: the files may
		// have grown since. Bring them up to date in place rather than throwing
		// the database away, which is the whole point of the file-state table.
		updated, changed, rerr := refresh(ctx, hit.DB, hit.Load, sources, load)
		if rerr != nil {
			// Falling back is always safe — a full re-read produces the same
			// data more slowly — and is far better than serving a half-updated
			// timeline during an incident.
			hit.DB.Close()
			os.Remove(path)
			return ingestFresh(ctx, sources, load, path,
				fmt.Sprintf("could not update cache, re-reading: %v", rerr))
		}

		hit.Load = updated
		if changed > 0 {
			hit.Hit = false
			hit.Reason = fmt.Sprintf("appended %s that changed", plural(changed, "file"))
			if err := writeCacheMeta(ctx, hit.DB, filepath.Base(path), updated); err != nil {
				hit.Reason = fmt.Sprintf("could not record cache metadata: %v", err)
			}
		}
		return hit, nil
	} else if err != nil {
		// A corrupt or unreadable cache file must never block the tool. Say so
		// and re-ingest over the top.
		os.Remove(path)
		return ingestFresh(ctx, sources, load, path, fmt.Sprintf("cache unusable, rebuilding: %v", err))
	}

	return ingestFresh(ctx, sources, load, path, "")
}

// openHit returns a Cached when the file exists and matches the fingerprint.
func openHit(ctx context.Context, path, fingerprint string) (*Cached, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, nil // No cache file yet, which is not an error.
	}

	db, err := Open(path)
	if err != nil {
		return nil, err
	}

	meta, err := readCacheMeta(ctx, db)
	if err != nil {
		db.Close()
		return nil, err
	}

	// The fingerprint is in the file name too, but checking the stored copy
	// catches a file that was renamed or half-written.
	if meta.Fingerprint != fingerprint || meta.Version != IngestVersion {
		db.Close()
		return nil, fmt.Errorf("stale cache (version %d, want %d)", meta.Version, IngestVersion)
	}

	// A cache that was never stamped complete is a cache whose ingest did not
	// finish. Reusing it would serve a partial result that looks healthy.
	if !meta.Complete {
		db.Close()
		return nil, fmt.Errorf("previous ingest did not finish")
	}

	if n, err := db.Count(ctx); err != nil || n == 0 {
		db.Close()
		return nil, fmt.Errorf("cache holds no records")
	}

	return &Cached{DB: db, Load: meta.Summary.toLoad(), Hit: true}, nil
}

// cacheMeta is what one cache file records about how it was built.
type cacheMeta struct {
	Summary     cachedSummary
	Version     int64
	Fingerprint string
	Complete    bool
}

func readCacheMeta(ctx context.Context, db *DB) (cacheMeta, error) {
	var (
		meta cacheMeta
		raw  string
	)

	row := db.QueryRow(ctx, `SELECT fingerprint, ingest_version, summary, complete FROM loupe_cache_meta LIMIT 1`)
	if err := row.Scan(&meta.Fingerprint, &meta.Version, &raw, &meta.Complete); err != nil {
		return cacheMeta{}, fmt.Errorf("read cache metadata: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &meta.Summary); err != nil {
		return cacheMeta{}, fmt.Errorf("decode cache metadata: %w", err)
	}
	return meta, nil
}

// MarkComplete stamps a cache file as safe to reuse.
//
// Appending the records is not the end of an ingest: schema inference runs
// afterwards and gives the frequent fields real columns, and until it has, the
// database on disk is missing every column a user would filter on. A run that
// died in between used to leave that half-built database behind under a name
// the next run trusted — same record count, same status line, no promoted
// columns — so `service:payments-api` came back as an unknown field and the
// honest conclusion was that the data does not carry it.
//
// Nothing reuses a file that has not been through here.
func (s *DB) MarkComplete(ctx context.Context) error {
	if err := s.Exec(ctx, `UPDATE loupe_cache_meta SET complete = true`); err != nil {
		return fmt.Errorf("mark cache complete: %w", err)
	}
	return nil
}

// ingestFresh reads the sources, writing to path when one is given.
func ingestFresh(ctx context.Context, sources []source.Source, load LoadOptions, path, reason string) (*Cached, error) {
	// Ingest into a temporary file and rename on success, so an interrupted
	// run never leaves a half-built database that a later run would trust.
	target, finalise := "", func() error { return nil }

	if path != "" {
		target = path + ".partial"
		os.Remove(target)
		finalise = func() error { return os.Rename(target, path) }
	}

	db, err := Open(target)
	if err != nil {
		return nil, err
	}

	result, err := db.Load(ctx, sources, load)
	if err != nil {
		db.Close()
		return nil, err
	}

	out := &Cached{DB: db, Load: result, Path: path, Reason: reason}

	// The per-file offsets are recorded whether or not this ingest is being
	// cached. A later run resumes from them, but so does follow mode inside
	// this one — and with no offsets a follower has nothing to resume from, so
	// its first poll plans a re-read of every file and republishes the whole
	// dataset as if it had just been written. In a live tail that is thousands
	// of lines the user has already read.
	if err := writeFileStates(ctx, db, statesFrom(sources, result)); err != nil {
		if target == "" {
			// Nothing to invalidate: this ingest was never going to be cached.
			// Following will re-read, which is wasteful but not wrong.
			out.Reason = fmt.Sprintf("could not record file offsets: %v", err)
			return out, nil
		}
		out.Path, out.Reason = "", fmt.Sprintf("could not record file offsets: %v", err)
		return out, nil
	}

	if target == "" {
		return out, nil
	}

	if err := writeCacheMeta(ctx, db, filepath.Base(path), result); err != nil {
		// Failing to record metadata makes the file unusable as a cache, but
		// the data in memory is fine, so carry on without caching.
		out.Path, out.Reason = "", fmt.Sprintf("could not write cache metadata: %v", err)
		return out, nil
	}

	// DuckDB flushes on close, so the file has to be closed before it can be
	// renamed into place and reopened.
	if err := db.Close(); err != nil {
		out.Path, out.Reason = "", fmt.Sprintf("could not finalise cache: %v", err)
		return ingestFresh(ctx, sources, load, "", out.Reason)
	}
	if err := finalise(); err != nil {
		os.Remove(target)
		return ingestFresh(ctx, sources, load, "", fmt.Sprintf("could not install cache: %v", err))
	}

	reopened, err := Open(path)
	if err != nil {
		return ingestFresh(ctx, sources, load, "", fmt.Sprintf("could not reopen cache: %v", err))
	}
	out.DB = reopened

	// Eviction failures are not worth reporting: the data the user asked for is
	// already in hand, and a cache that is too large is a tidiness problem.
	_, _, _ = PruneCache(filepath.Dir(path), DefaultCacheLimit, path)

	return out, nil
}

func writeCacheMeta(ctx context.Context, db *DB, fileName string, result Load) error {
	fingerprint := strings.TrimSuffix(fileName, ".duckdb")

	if err := db.Exec(ctx, cacheMetaTable); err != nil {
		return err
	}

	summary := cachedSummary{
		Results: result.Results,
		Stats: parseStatsJSON{
			Lines:        result.Stats.Lines,
			Records:      result.Stats.Records,
			Unparsed:     result.Stats.Unparsed,
			NoTimestamp:  result.Stats.NoTimestamp,
			Continuation: result.Stats.Continuation,
			Truncated:    result.Stats.Truncated,
			Blank:        result.Stats.Blank,
			ZoneAssumed:  result.Stats.ZoneAssumed,
			InvalidUTF8:  result.Stats.InvalidUTF8,
		},
		Took: result.Took,
	}

	encoded, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("encode cache metadata: %w", err)
	}

	// One row, by definition. An incremental refresh rewrites this after
	// appending, and without the delete the table would accumulate rows while
	// readCacheMeta takes an unordered LIMIT 1 — serving the counts from before
	// the append, which is precisely the "quieter on the second run" failure
	// the summary exists to prevent.
	if err := db.Exec(ctx, `DELETE FROM loupe_cache_meta`); err != nil {
		return fmt.Errorf("clear cache metadata: %w", err)
	}

	// complete is false here on purpose. Writing the records is not the whole
	// ingest — see MarkComplete.
	return db.Exec(ctx,
		`INSERT INTO loupe_cache_meta VALUES (?, ?, ?, ?, ?)`,
		fingerprint, int64(IngestVersion), time.Now().UTC(), string(encoded), false)
}

func (s cachedSummary) toLoad() Load {
	load := Load{Results: s.Results, Took: s.Took}
	load.Stats.Lines = s.Stats.Lines
	load.Stats.Records = s.Stats.Records
	load.Stats.Unparsed = s.Stats.Unparsed
	load.Stats.NoTimestamp = s.Stats.NoTimestamp
	load.Stats.Continuation = s.Stats.Continuation
	load.Stats.Truncated = s.Stats.Truncated
	load.Stats.Blank = s.Stats.Blank
	load.Stats.ZoneAssumed = s.Stats.ZoneAssumed
	load.Stats.InvalidUTF8 = s.Stats.InvalidUTF8
	return load
}

// CacheEntry describes one cached database on disk.
type CacheEntry struct {
	Path     string
	Size     int64
	Modified time.Time
}

// ListCache returns the cached databases, newest first.
func ListCache(dir string) ([]CacheEntry, error) {
	resolved, err := CacheDir(dir)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read cache directory: %w", err)
	}

	var out []CacheEntry
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".duckdb" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, CacheEntry{
			Path:     filepath.Join(resolved, e.Name()),
			Size:     info.Size(),
			Modified: info.ModTime(),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

// DefaultCacheLimit caps the total size of the cache directory.
//
// Each entry is tens of megabytes and every distinct directory state leaves one
// behind, so without a cap the directory grows until somebody notices.
const DefaultCacheLimit = 2 << 30 // 2GiB

// DefaultRetention is how long a cached ingest survives without being used.
//
// An unsubscribed location keeps its cache for a fortnight, so re-subscribing
// during an incident is instant rather than a re-read. After that it is stale
// enough that the files have probably changed anyway.
const DefaultRetention = 14 * 24 * time.Hour

// PruneCache deletes the least recently modified entries until the cache fits
// within limit, and returns what it removed.
//
// keep is never deleted, so the entry just written survives even when it alone
// exceeds the limit — evicting the result of the run in progress would mean the
// next run re-ingests, forever.
func PruneCache(dir string, limit int64, keep string) (removed int, freed int64, err error) {
	if limit <= 0 {
		limit = DefaultCacheLimit
	}

	entries, err := ListCache(dir)
	if err != nil {
		return 0, 0, err
	}

	// Age first, then size. An entry nobody has opened for a fortnight goes
	// whether or not the cache is near its cap.
	cutoff := time.Now().Add(-DefaultRetention)
	kept := entries[:0]

	for _, e := range entries {
		if e.Path != keep && e.Modified.Before(cutoff) {
			if os.Remove(e.Path) == nil {
				removed++
				freed += e.Size
				continue
			}
		}
		kept = append(kept, e)
	}
	entries = kept

	var total int64
	for _, e := range entries {
		total += e.Size
	}

	// ListCache returns newest first, so walking backwards evicts the coldest.
	for i := len(entries) - 1; i >= 0 && total > limit; i-- {
		if entries[i].Path == keep {
			continue
		}
		if rmErr := os.Remove(entries[i].Path); rmErr != nil {
			// A file we cannot remove is not worth failing the run over; the
			// data the user asked for is already in hand.
			continue
		}
		total -= entries[i].Size
		freed += entries[i].Size
		removed++
	}

	return removed, freed, nil
}

// ClearCache removes every cached database and returns how many and how much.
func ClearCache(dir string) (removed int, freed int64, err error) {
	entries, err := ListCache(dir)
	if err != nil {
		return 0, 0, err
	}

	for _, e := range entries {
		if rmErr := os.Remove(e.Path); rmErr != nil {
			return removed, freed, fmt.Errorf("remove %s: %w", e.Path, rmErr)
		}
		removed++
		freed += e.Size
	}
	return removed, freed, nil
}
