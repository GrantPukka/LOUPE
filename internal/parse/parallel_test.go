package parse

import (
	"bytes"
	"os"
	"reflect"
	"testing"
	"time"
)

// Reading in parallel must be indistinguishable from reading serially.
//
// Not "close enough": identical. seq is assigned in emit order and is the
// stable sort key for every listing, the Tail is where a resumed read picks up,
// and the stats are what the status line reports — a parallel read that got any
// of them subtly wrong would produce a database that disagrees with itself
// depending on how many cores the machine had.
//
// So this compares the whole observable output of the two paths, over every
// fixture in the tree and at several worker counts.
func TestParallelReadMatchesSerial(t *testing.T) {
	for _, path := range fixtureLogPaths(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		for _, name := range Names() {
			parser, _ := Get(name)
			loc := time.FixedZone("TEST", 5*3600)

			want, wantTail, wantErr := collectRead(data, parser, loc, 0)

			for _, workers := range []int{1, 2, 3, 8} {
				got, gotTail, gotErr := collectRead(data, parser, loc, workers)

				switch {
				case (wantErr == nil) != (gotErr == nil):
					t.Errorf("%s/%s workers=%d: error mismatch: serial %v, parallel %v",
						path, name, workers, wantErr, gotErr)
				case !reflect.DeepEqual(wantTail, gotTail):
					t.Errorf("%s/%s workers=%d: tail differs\n serial   %+v\n parallel %+v",
						path, name, workers, wantTail, gotTail)
				case len(want.entries) != len(got.entries):
					t.Errorf("%s/%s workers=%d: %d records serial, %d parallel",
						path, name, workers, len(want.entries), len(got.entries))
				case !want.stats.Equal(got.stats):
					t.Errorf("%s/%s workers=%d: stats differ\n serial   %+v\n parallel %+v",
						path, name, workers, want.stats, got.stats)
				default:
					for i := range want.entries {
						if !reflect.DeepEqual(want.entries[i], got.entries[i]) {
							t.Errorf("%s/%s workers=%d: record %d differs\n serial   %+v\n parallel %+v",
								path, name, workers, i, want.entries[i], got.entries[i])
							break
						}
					}
				}
			}
		}
	}
}

type readResult struct {
	entries []Entry
	stats   Stats
}

func collectRead(data []byte, p Parser, loc *time.Location, workers int) (readResult, Tail, error) {
	var out readResult
	stats, tail, err := ReadAll(bytes.NewReader(data),
		ReaderOptions{Parser: p, Loc: loc, Workers: workers},
		func(e Entry) error {
			out.entries = append(out.entries, e)
			return nil
		})
	out.stats = stats
	return out, tail, err
}

func fixtureLogPaths(t *testing.T) []string {
	t.Helper()

	var out []string
	for _, line := range everyFixtureFile(t) {
		out = append(out, line)
	}
	if len(out) == 0 {
		t.Fatal("no fixtures found; this test needs them to mean anything")
	}
	return out
}
