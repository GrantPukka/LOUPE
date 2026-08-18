package session

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GrantPukka/loupe/internal/blaster"
)

// update regenerates the golden file instead of comparing against it:
//
//	go test ./internal/session -run TestPatternsGolden -update
//
// Always read `git diff testdata/` afterwards. The golden file is the actual
// test; regenerating without reading the diff turns a failing test green
// without anyone deciding the new output is correct — and for this test the
// diff is the collapse rule itself, which is the thing most worth reviewing.
var update = flag.Bool("update", false, "regenerate golden files")

// goldenPattern is the on-disk shape: verbose and stable rather than compact,
// because a reviewer reads this diff to decide whether a masking change was an
// improvement or a regression.
type goldenPattern struct {
	ID       string   `json:"id"`
	Template string   `json:"template"`
	Count    int64    `json:"count"`
	Sources  []string `json:"sources"`
	Example  string   `json:"example"`
	Unparsed bool     `json:"unparsed,omitempty"`
}

type goldenListing struct {
	Records           int64           `json:"records"`
	Templates         int64           `json:"templates"`
	UnparsedTemplates int64           `json:"unparsed_templates"`
	UnparsedRecords   int64           `json:"unparsed_records"`
	Patterns          []goldenPattern `json:"patterns"`
}

// demoCorpus writes the same six formats and the same deliberate noise that
// `loupe demo` produces.
//
// Generated rather than checked in: the demo directory is gitignored, so a
// test that read it would pass on the machine that had run `make demo` and
// skip everywhere else. A fixed seed makes the blaster byte-identical run to
// run, which is what the golden file needs.
func demoCorpus(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	err := blaster.Run(blaster.Config{
		Out:      dir,
		Seed:     42,
		Scenario: "incident",
		Duration: 2 * time.Minute,
		Rate:     12,
		Malform:  0.015,
		Rotate:   false,
	})
	if err != nil {
		t.Fatalf("generate corpus: %v", err)
	}
	return dir
}

// TestPatternsGolden pins the whole collapse rule against a realistic corpus.
//
// The unit tests check one rule at a time. This checks what they add up to on
// six formats with real noise in them, which is where over-collapsing would
// actually show up: two templates silently becoming one is invisible in a
// rule-by-rule test and obvious in this diff.
func TestPatternsGolden(t *testing.T) {
	dir := demoCorpus(t)

	sess, err := Open(context.Background(), Options{
		Paths:    []string{dir},
		Location: time.UTC,
		NoCache:  true,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close()

	set, err := sess.Patterns(context.Background(), plan(t, sess, ""), PatternQuery{Limit: -1})
	if err != nil {
		t.Fatalf("Patterns: %v", err)
	}

	got := goldenListing{
		Records:           set.Records,
		Templates:         set.Templates,
		UnparsedTemplates: set.UnparsedTemplates,
		UnparsedRecords:   set.UnparsedRecords,
	}
	for _, p := range set.Patterns {
		got.Patterns = append(got.Patterns, goldenPattern{
			ID:       p.ID,
			Template: p.Template,
			Count:    p.Count,
			Sources:  p.Sources,
			Example:  p.Example,
		})
	}

	encoded, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	encoded = append(encoded, '\n')

	path := filepath.Join("testdata", "patterns.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("create testdata: %v", err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote %s (%d templates)", path, len(got.Patterns))
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}

	if string(encoded) != string(want) {
		t.Errorf("pattern listing does not match %s.\n"+
			"If the change is intended, rerun with -update and read the diff.\n"+
			"got %d templates over %d records, golden has a different shape",
			path, len(got.Patterns), got.Records)
	}
}
