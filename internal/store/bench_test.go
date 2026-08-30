package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GrantPukka/loupe/internal/blaster"
	"github.com/GrantPukka/loupe/internal/source"
)

// The benchmark corpus. Big enough that per-record work shows up above the
// noise, small enough to generate in a couple of seconds.
const (
	benchDuration = 10 * time.Minute
	benchRate     = 120
)

// benchCorpus generates a realistic mixed-format directory.
//
// The blaster rather than a synthetic loop: ingest cost depends on which
// parsers run, how many lines are malformed, and how many carry continuation
// lines. A million copies of one JSON line would measure none of that.
func benchCorpus(tb testing.TB, dir string) int64 {
	tb.Helper()

	err := blaster.Run(blaster.Config{
		Out:      dir,
		Seed:     42,
		Scenario: "incident",
		Duration: benchDuration,
		Rate:     benchRate,
		Malform:  0.015,
		Rotate:   false,
	})
	if err != nil {
		tb.Fatalf("generate corpus: %v", err)
	}

	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		tb.Fatalf("read corpus: %v", err)
	}
	for _, e := range entries {
		if info, err := os.Stat(filepath.Join(dir, e.Name())); err == nil {
			total += info.Size()
		}
	}
	return total
}

// BenchmarkIngest measures the whole read-parse-append path.
//
// This is the number CLAUDE.md's ingest rates are measured from, so a change to
// the ingest path is expected to report it before and after. The rates there
// are what this path actually does — about 5MB/s on one format and 2MB/s on a
// merged stream — not a target it is being held to.
func BenchmarkIngest(b *testing.B) {
	dir := b.TempDir()
	bytes := benchCorpus(b, dir)

	sources, err := source.Walk(dir, nil)
	if err != nil {
		b.Fatalf("walk: %v", err)
	}

	ctx := context.Background()
	b.SetBytes(bytes)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		db, err := Open("")
		if err != nil {
			b.Fatalf("open: %v", err)
		}

		load, err := db.Load(ctx, sources, LoadOptions{})
		if err != nil {
			b.Fatalf("load: %v", err)
		}

		b.StopTimer()
		if load.Stats.Records == 0 {
			b.Fatal("corpus produced no records")
		}
		db.Close()
		b.StartTimer()
	}
}
