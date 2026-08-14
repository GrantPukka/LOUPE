package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/VIGIL-OPS/loupe/internal/parse"
	"github.com/VIGIL-OPS/loupe/internal/source"
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
	var order []Source

	for _, r := range l.Results {
		if r.Stats.ZoneAssumed == 0 {
			continue
		}
		if _, seen := counts[r.Source.Name]; !seen {
			order = append(order, r.Source)
		}
		counts[r.Source.Name] += r.Stats.ZoneAssumed
	}

	out := make([]AssumedZone, 0, len(order))
	for _, s := range order {
		out = append(out, AssumedZone{Source: s, Records: counts[s.Name]})
	}
	return out
}

// AssumedZone is one source's reliance on an assumed timezone.
type AssumedZone struct {
	Source  Source
	Records int64
}

// Load ingests sources into the store.
func (s *DB) Load(ctx context.Context, sources []source.Source, opts LoadOptions) (Load, error) {
	start := time.Now()
	var load Load

	ing, err := s.NewIngester()
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

	rc, err := src.Open(ctx)
	if err != nil {
		return IngestResult{}, fmt.Errorf("open %s: %w", src.Name(), err)
	}
	defer rc.Close()

	var ingestErr error
	stats, err := parse.ReadAll(rc, parse.ReaderOptions{Parser: parser, Loc: loc}, func(e parse.Entry) error {
		if err := ing.Add(e); err != nil {
			ingestErr = err
			return err
		}
		return nil
	})
	if ingestErr != nil {
		return IngestResult{}, ingestErr
	}
	if err != nil {
		return IngestResult{}, fmt.Errorf("read %s: %w", src.Name(), err)
	}

	return IngestResult{Source: meta, Stats: stats, Took: time.Since(start)}, nil
}

// parserFor resolves the format for a source, either from the override or by
// sampling the head of the file.
func (s *DB) parserFor(ctx context.Context, src source.Source, override string) (parse.Parser, error) {
	if override != "" {
		p, ok := parse.Get(override)
		if !ok {
			return nil, fmt.Errorf("unknown parser %q (available: %s)",
				override, strings.Join(parse.Names(), ", "))
		}
		return p, nil
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

	det := parse.Detect(sample)
	if det.Parser == nil {
		return nil, fmt.Errorf("no parser could read %s", src.Name())
	}
	return det.Parser, nil
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
// checkout-api.log, checkout-api.log.1, and checkout-api.log.2.gz all become
// checkout-api, which is what makes source:checkout-api match a rotation group.
func logicalName(path string) string {
	name := filepath.Base(path)
	name = strings.TrimSuffix(name, ".gz")

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
