package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GrantPukka/loupe/internal/parse"
	"github.com/GrantPukka/loupe/internal/source"
)

// LoadOptions configures an ingest run.
type LoadOptions struct {
	// Parser forces a format instead of detecting one. Empty means detect.
	Parser string

	// SourceZones maps a logical source name to the timezone assumed for it
	// when its format carries none. The empty key is the default for every
	// source, and defaults to UTC.
	SourceZones map[string]*time.Location

	// Progress, when set, is called after each source is ingested.
	Progress func(IngestResult)

	// Table overrides the destination table. Empty means logs. Follow mode
	// stages records elsewhere first, because the appender writes the base
	// column set and logs has been widened by schema inference.
	Table string

	// Resume, when set, maps a source's name to where a previous read of it
	// stopped. A source listed here is read from that offset instead of from
	// the beginning; one absent from it is read whole, which is what makes a
	// first run and a newly appeared file behave identically.
	Resume map[string]Resume

	// OnBatch, when set, is called during the read with the sequence number
	// the batch began at. Everything below that point has been flushed and is
	// visible to a query.
	//
	// This is what makes a stream usable. A pipe from `kubectl logs -f` never
	// reaches EOF, so an ingest that only reported at the end would sit there
	// producing nothing for as long as the pod lived — a hang, as far as the
	// person watching is concerned.
	OnBatch func(from int64) error
}

// Batching thresholds for OnBatch.
//
// Whichever comes first: enough records to be worth a flush, or enough time
// that a slow stream should not be sitting on them. A quiet pipe emits its
// first record immediately, because the elapsed check is measured from a zero
// time on the first record and always trips.
const (
	batchRecords  = 500
	batchInterval = 200 * time.Millisecond
)

// Resume is where a previous read of a file stopped.
//
// Offset is a record boundary, not a line boundary or the end of the file —
// see parse.ReadAll. LastLine carries the physical line number forward so that
// records ingested by a later read continue the file's numbering rather than
// restarting at 1, which would make line_no ambiguous within one file.
type Resume struct {
	Offset   int64
	LastLine int64
}

// Load reads every source into the store.
//
// Errors reading one source do not abort the others: a directory with one
// unreadable file should still produce a timeline of the rest, with the failure
// reported. Only a failure to write to the store is fatal.
type Load struct {
	Results []IngestResult
	Errors  []error
	Stats   parse.Stats
	Took    time.Duration
}

// Sources returns the distinct logical sources ingested, in ingest order.
func (l Load) Sources() []Source {
	seen := map[string]bool{}
	var out []Source
	for _, r := range l.Results {
		if seen[r.Source.Name] {
			continue
		}
		seen[r.Source.Name] = true
		out = append(out, r.Source)
	}
	return out
}

// AssumedZones lists the sources that actually contain timestamps resolved
// with an assumed timezone, with the count for each.
//
// This reports what happened, not what was configured. A source whose format
// always carries an offset never appears here, however the flags were set, and
// a source that carries offsets on most lines but not all does — which is the
// case that silently corrupts an investigation.
func (l Load) AssumedZones() []AssumedZone {
	counts := map[string]int64{}
	abbrevs := map[string]map[string]int64{}
	var order []Source

	for _, r := range l.Results {
		if r.Stats.ZoneAssumed == 0 {
			continue
		}
		name := r.Source.Name
		if _, seen := counts[name]; !seen {
			order = append(order, r.Source)
		}
		counts[name] += r.Stats.ZoneAssumed
		for abbrev, n := range r.Stats.ZoneAbbrevs {
			if abbrevs[name] == nil {
				abbrevs[name] = map[string]int64{}
			}
			abbrevs[name][abbrev] += n
		}
	}

	out := make([]AssumedZone, 0, len(order))
	for _, s := range order {
		out = append(out, AssumedZone{
			Source:  s,
			Records: counts[s.Name],
			Abbrevs: sortedAbbrevs(abbrevs[s.Name]),
		})
	}
	return out
}

// sortedAbbrevs orders the abbreviations by how many records carried them, so
// the one that matters leads. Ties break by name to keep output stable.
func sortedAbbrevs(counts map[string]int64) []string {
	out := make([]string, 0, len(counts))
	for abbrev := range counts {
		out = append(out, abbrev)
	}
	sort.Slice(out, func(i, j int) bool {
		if counts[out[i]] != counts[out[j]] {
			return counts[out[i]] > counts[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// AssumedZone is one source's reliance on an assumed timezone.
type AssumedZone struct {
	Source  Source
	Records int64

	// Abbrevs are the zone abbreviations those records wrote, when they wrote
	// one that could not be resolved — most often a Postgres log saying AEST.
	// Empty means the format carried no zone at all, which is a different and
	// less actionable situation.
	Abbrevs []string
}

// Load ingests sources into the store.
func (s *DB) Load(ctx context.Context, sources []source.Source, opts LoadOptions) (Load, error) {
	start := time.Now()
	var load Load

	table := opts.Table
	if table == "" {
		table = "logs"
	}
	ing, err := s.NewIngesterInto(table)
	if err != nil {
		return load, err
	}
	// Close is checked explicitly below rather than deferred and ignored:
	// buffered rows are lost on a failed close, and losing records silently is
	// the failure mode this project exists to prevent.
	defer func() {
		if ing != nil {
			ing.Close()
		}
	}()

	// Checked once, before anything is read. Left to the per-source path this
	// surfaces as a warning on every file and a listing of no records at all —
	// a typo answering with silence, which is the behaviour FILTER-DSL section
	// 7 forbids for a field name and which is no better for a format name.
	if opts.Parser != "" {
		if _, ok := parse.Get(opts.Parser); !ok {
			return load, fmt.Errorf("unknown parser %q (available: %s)",
				opts.Parser, strings.Join(parse.Names(), ", "))
		}
	}

	if err := checkSourceZones(opts.SourceZones, sources); err != nil {
		return load, err
	}

	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			return load, err
		}

		result, err := s.loadOne(ctx, ing, src, opts)
		if err != nil {
			load.Errors = append(load.Errors, err)
			continue
		}

		load.Results = append(load.Results, result)
		load.Stats.Add(result.Stats)
		if opts.Progress != nil {
			opts.Progress(result)
		}
	}

	closer := ing
	ing = nil
	if err := closer.Close(); err != nil {
		return load, err
	}

	load.Took = time.Since(start)
	return load, nil
}

// readWorkers is how many goroutines parse head lines for one source.
//
// Parsing was 60% of a merged-corpus ingest and ran on one core; it is pure
// per-record work and the obvious thing to spread out. Deciding where records
// begin, counting them and emitting them stay sequential — see
// parse.ReaderOptions.Workers.
//
// Not while a batch callback is set. That is live mode, where the point is to
// show a record the moment it arrives, and a pool holds one back until its
// batch fills. A stream trades throughput for latency by definition.
func readWorkers(opts LoadOptions) int {
	if opts.OnBatch != nil {
		return 1
	}
	return runtime.GOMAXPROCS(0)
}

// checkSourceZones rejects a --source-tz that names a source nothing is called.
//
// It used to be ignored in silence: the map was only ever read by name, so
// `--source-tz postgres:Australia/Brisbane` on a file called platform-mixed
// matched nothing, changed nothing, and the status line went on reporting
// "read as UTC (default)". The only way to notice was to already know the
// right answer, which is the opposite of what an escape hatch is for.
//
// docs/FILTER-DSL.md section 7 requires an unknown field name to be an error
// naming what is available rather than an empty result. The same rule belongs
// here, and for the same reason: silence is indistinguishable from success.
//
// The name is the *source* — for a directory, usually the file's base name with
// its rotation suffix removed. It is not the format: one merged file is a
// single source carrying a dozen formats, which the message says outright,
// because reading it as a format name is the mistake the old help text invited.
func checkSourceZones(zones map[string]*time.Location, sources []source.Source) error {
	if len(zones) == 0 {
		return nil
	}

	known := make(map[string]bool, len(sources))
	names := make([]string, 0, len(sources))
	for _, src := range sources {
		if name := logicalName(src.Name()); !known[name] {
			known[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)

	for name := range zones {
		// The empty key is the bare --source-tz=ZONE form, which names no
		// source because it applies to all of them.
		if name == "" || known[name] {
			continue
		}
		if parse.Names() != nil && isParserName(name) {
			return fmt.Errorf("--source-tz names the source %q, but %q is a log format, not a source; "+
				"sources here are: %s", name, name, strings.Join(names, ", "))
		}
		return fmt.Errorf("--source-tz names the source %q, which was not read; sources here are: %s",
			name, strings.Join(names, ", "))
	}
	return nil
}

// isParserName reports whether a name is a registered log format, so that
// --source-tz postgres:… can say what is actually wrong rather than only that
// no source matched.
func isParserName(name string) bool {
	_, ok := parse.Get(name)
	return ok
}

func (s *DB) loadOne(ctx context.Context, ing *Ingester, src source.Source, opts LoadOptions) (IngestResult, error) {
	start := time.Now()

	parser, err := s.parserFor(ctx, src, opts.Parser)
	if err != nil {
		return IngestResult{}, err
	}

	loc, zoneOrigin := zoneFor(logicalName(src.Name()), opts.SourceZones)

	meta := Source{
		Name:       logicalName(src.Name()),
		File:       src.Name(),
		Format:     parser.Name(),
		ZoneSource: zoneOrigin,
		Zone:       loc.String(),
	}
	ing.SetSource(meta)

	resume := opts.Resume[src.Name()]

	rc, err := openAt(ctx, src, resume.Offset)
	if err != nil {
		return IngestResult{}, err
	}
	defer rc.Close()

	var (
		ingestErr error
		batchFrom = s.seq
		batchN    int
		lastFlush time.Time
	)

	stats, tail, err := parse.ReadAll(rc,
		parse.ReaderOptions{
			Parser:    parser,
			Loc:       loc,
			StartLine: resume.LastLine,
			Workers:   readWorkers(opts),
		},
		func(e parse.Entry) error {
			if err := ing.Add(e); err != nil {
				ingestErr = err
				return err
			}
			if opts.OnBatch == nil {
				return nil
			}

			batchN++
			if batchN < batchRecords && time.Since(lastFlush) < batchInterval {
				return nil
			}
			if err := ing.Flush(); err != nil {
				ingestErr = err
				return err
			}
			if err := opts.OnBatch(batchFrom); err != nil {
				ingestErr = err
				return err
			}
			batchFrom, batchN, lastFlush = s.seq, 0, time.Now()
			return nil
		})
	if ingestErr != nil {
		return IngestResult{}, ingestErr
	}
	if err != nil {
		return IngestResult{}, fmt.Errorf("read %s: %w", src.Name(), err)
	}

	// Whatever the last batch did not reach. A stream that ends between
	// thresholds must not leave its final records unshown.
	if opts.OnBatch != nil && batchN > 0 {
		if err := ing.Flush(); err != nil {
			return IngestResult{}, err
		}
		if err := opts.OnBatch(batchFrom); err != nil {
			return IngestResult{}, err
		}
	}

	// Sanitisation happens at the appender, so its count is collected here
	// rather than arriving from the reader with the rest.
	stats.InvalidUTF8 = ing.InvalidUTF8()

	return IngestResult{
		Source: meta,
		Stats:  stats,
		Took:   time.Since(start),
		// tail.Offset is relative to where this read started.
		ResumeAt:   resume.Offset + tail.Offset,
		ResumeLine: tail.Line,
		Before:     tail.Before,
	}, nil
}

// sampleFor reads enough of a source to detect its format.
//
// A source that can be peeked is peeked, because detection happens before the
// ingest reads the same source again. A file can be opened twice and is; a
// stream cannot, and sampling one by reading it would consume the first two
// hundred lines and lose them — records gone before anything counted them.
func sampleFor(ctx context.Context, src source.Source) ([][]byte, error) {
	if p, ok := src.(source.Peekable); ok {
		head, err := p.Peek(source.PeekBytes)
		if err != nil {
			return nil, fmt.Errorf("sample %s: %w", src.Name(), err)
		}

		// The window almost certainly ends mid-line. Detection scores whole
		// lines, so the fragment is dropped rather than counted as a line that
		// no parser understood.
		if cut := bytes.LastIndexByte(head, '\n'); cut >= 0 {
			head = head[:cut+1]
		}

		sample, err := parse.SampleLines(bytes.NewReader(head), 0)
		if err != nil {
			return nil, fmt.Errorf("sample %s: %w", src.Name(), err)
		}
		return sample, nil
	}

	rc, err := src.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("open %s for detection: %w", src.Name(), err)
	}
	defer rc.Close()

	sample, err := parse.SampleLines(rc, 0)
	if err != nil {
		return nil, fmt.Errorf("sample %s: %w", src.Name(), err)
	}
	return sample, nil
}

// openAt opens a source, from offset when one is given.
//
// A source that cannot be resumed is read from the start. That is the safe
// direction: re-reading costs time, whereas skipping bytes we cannot seek past
// would silently drop records.
func openAt(ctx context.Context, src source.Source, offset int64) (io.ReadCloser, error) {
	if offset > 0 {
		if t, ok := src.(source.Tailable); ok {
			rc, err := t.OpenAt(ctx, offset)
			if err != nil {
				return nil, fmt.Errorf("open %s at %d: %w", src.Name(), offset, err)
			}
			return rc, nil
		}
	}

	rc, err := src.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", src.Name(), err)
	}
	return rc, nil
}

// MixedCoverage is the share of sampled lines the detected parser must claim
// before it is trusted with the whole file.
//
// Below it, the file is read with per-line detection instead. The threshold is
// deliberately generous: a file that really is one format with a scattering of
// damaged lines sits far above it, and a file that is genuinely mixed sits far
// below — the corpus that prompted this had the detected parser claiming 15.5%.
// Choosing per-line detection when it was not needed costs some ingest speed
// and nothing else, because the per-line answer for a uniform file is the same
// parser on every line.
const MixedCoverage = 0.8

// MixedMinSample is how many lines must be seen before coverage means anything.
//
// A file of three lines, one of them damaged, scores 0.67 and would otherwise
// be declared multi-format on no evidence at all. Below this, detection is
// trusted; the fallback for a genuinely mixed file that is this short is that
// its handful of records are unparsed, which is what they would have been
// anyway.
const MixedMinSample = 20

// parserFor resolves the format for a source, either from the override or by
// sampling the head of the file.
//
// Detection answers "which parser is this file's format", and then coverage
// answers the question that was missing: "and does that parser actually read
// this file". A file where it does not is read per line instead. See
// parse.mixedParser for why one file is not necessarily one format.
func (s *DB) parserFor(ctx context.Context, src source.Source, override string) (parse.Parser, error) {
	if override != "" {
		p, ok := parse.Get(override)
		if !ok {
			return nil, fmt.Errorf("unknown parser %q (available: %s)",
				override, strings.Join(parse.Names(), ", "))
		}
		return p, nil
	}

	sample, err := sampleFor(ctx, src)
	if err != nil {
		return nil, err
	}

	det := parse.Detect(sample)
	if det.Parser == nil {
		return nil, fmt.Errorf("no parser could read %s", src.Name())
	}

	if needsPerLineDetection(det.Parser, sample) {
		if mixed, ok := parse.Get(parse.MixedName); ok {
			return mixed, nil
		}
	}
	return det.Parser, nil
}

// needsPerLineDetection reports whether the detected parser leaves too much of
// the sample unread to be trusted with the file.
//
// The fallback is asked a different question, because it claims every line and
// so has perfect coverage by construction. What matters for it is whether a
// real parser would have claimed some of the lines — if one would, the file has
// structure the fallback is about to throw away.
func needsPerLineDetection(detected parse.Parser, sample [][]byte) bool {
	if len(sample) < MixedMinSample {
		return false
	}

	if detected.Name() == "text" {
		for name, n := range parse.Formats(sample) {
			if name != "text" && n > 0 {
				return true
			}
		}
		return false
	}

	return parse.Coverage(detected, sample) < MixedCoverage
}

// zoneFor picks the timezone applied to this source's zoneless timestamps, and
// reports where that choice came from so the user can audit it.
//
// The default is UTC, not the local zone: servers overwhelmingly run UTC, and
// the wrong default here is worse than a slightly surprising one.
//
// Note that this is only the zone that *would* be applied. Whether it actually
// was is a per-record fact, counted as Stats.ZoneAssumed, because a jsonl
// source can carry offsets on most lines and omit them on a few.
func zoneFor(name string, zones map[string]*time.Location) (*time.Location, ZoneOrigin) {
	if zones != nil {
		if loc, ok := zones[name]; ok && loc != nil {
			return loc, ZoneFromFlagPerSource
		}
		if loc, ok := zones[""]; ok && loc != nil {
			return loc, ZoneFromFlag
		}
	}
	return time.UTC, ZoneFromDefault
}

// logicalName reduces a path to the source name a user would type, dropping the
// directory, the compression suffix, the rotation number, and a trailing .log.
//
// checkout-api.log, checkout-api.log.1, and checkout-api.log.2.zst all become
// checkout-api, which is what makes source:checkout-api match a rotation group
// whatever its archives are compressed with.
func logicalName(path string) string {
	// The same trimming the walker uses to group a rotation, so a source name
	// and a rotation group cannot disagree about which files belong together.
	name := source.TrimCompressionSuffix(filepath.Base(path))

	if i := strings.LastIndex(name, "."); i > 0 {
		if _, err := strconv.Atoi(name[i+1:]); err == nil {
			name = name[:i]
		}
	}

	for _, ext := range []string{".log", ".txt", ".out", ".err"} {
		if strings.HasSuffix(name, ext) {
			return strings.TrimSuffix(name, ext)
		}
	}
	return name
}
