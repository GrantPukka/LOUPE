package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gen runs the generator into a temp directory and returns the path.
func gen(t *testing.T, c config) string {
	t.Helper()
	c.out = t.TempDir()
	if err := run(c); err != nil {
		t.Fatalf("run: %v", err)
	}
	return c.out
}

func defaults() config {
	return config{
		scenario: "incident",
		duration: 2 * time.Minute,
		rate:     8,
		seed:     7,
		malform:  0.02,
		rotate:   true,
	}
}

// digest hashes every file in a directory, so two runs can be compared as a
// whole rather than file by file.
func digest(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		sum := sha256.Sum256(b)
		out[e.Name()] = hex.EncodeToString(sum[:])
	}
	return out
}

// The same seed must produce byte-identical output. Golden-file fixtures are
// regenerated with this tool, so any nondeterminism turns every fixture diff
// into noise and the golden tests stop being able to catch anything.
func TestSameSeedIsByteIdentical(t *testing.T) {
	a := digest(t, gen(t, defaults()))
	b := digest(t, gen(t, defaults()))

	if len(a) == 0 {
		t.Fatal("no files generated")
	}
	if len(a) != len(b) {
		t.Fatalf("file count differs: %d vs %d", len(a), len(b))
	}
	for name, sumA := range a {
		sumB, ok := b[name]
		if !ok {
			t.Errorf("%s missing from second run", name)
			continue
		}
		if sumA != sumB {
			t.Errorf("%s differs between runs with the same seed", name)
		}
	}
}

func TestDifferentSeedDiffers(t *testing.T) {
	c := defaults()
	a := digest(t, gen(t, c))
	c.seed = 99
	b := digest(t, gen(t, c))

	if a["checkout-api.log"] == b["checkout-api.log"] {
		t.Error("different seeds produced identical output; the seed is not wired through")
	}
}

func readManifest(t *testing.T, dir string) manifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	return m
}

// The manifest is what parser tests assert against, so its counts must match
// the files on disk exactly. A parser that drops records is supposed to fail
// against these numbers.
func TestManifestMatchesFilesOnDisk(t *testing.T) {
	dir := gen(t, defaults())
	m := readManifest(t, dir)

	if len(m.Files) != len(sinks) {
		t.Fatalf("manifest lists %d files, want %d", len(m.Files), len(sinks))
	}

	for _, r := range m.Files {
		b, err := os.ReadFile(filepath.Join(dir, r.File))
		if err != nil {
			t.Errorf("read %s: %v", r.File, err)
			continue
		}
		// Every record is written with a trailing newline, so the physical line
		// count is the newline count.
		got := strings.Count(string(b), "\n")
		if got != r.Lines {
			t.Errorf("%s: manifest says %d lines, file has %d", r.File, r.Lines, got)
		}
		if r.Records == 0 {
			t.Errorf("%s: no records generated", r.File)
		}
		if r.Records > r.Lines {
			t.Errorf("%s: %d records in %d lines; records cannot exceed lines",
				r.File, r.Records, r.Lines)
		}
	}
}

// Log4j stack traces are the multi-line case. If lines never exceeds records
// for that source, no continuation lines were emitted and the parser test for
// multi-line handling is vacuous.
func TestLog4jEmitsMultiLineRecords(t *testing.T) {
	dir := gen(t, defaults())
	m := readManifest(t, dir)

	for _, r := range m.Files {
		if r.Format != "log4j" {
			continue
		}
		if r.Lines <= r.Records {
			t.Errorf("%s: %d lines for %d records, expected continuation lines",
				r.File, r.Lines, r.Records)
		}
		b, err := os.ReadFile(filepath.Join(dir, r.File))
		if err != nil {
			t.Fatalf("read %s: %v", r.File, err)
		}
		if !strings.Contains(string(b), "\n\tat com.acme.pay.GatewayClient.charge") {
			t.Error("no Java stack trace continuation lines found")
		}
		return
	}
	t.Fatal("no log4j source in manifest")
}

// Damaged lines are the whole reason this generator exists. Fixtures without
// them cannot test the never-lose-data-silently principle.
func TestMalformedLinesAreGenerated(t *testing.T) {
	dir := gen(t, defaults())
	m := readManifest(t, dir)

	var broken int
	for _, r := range m.Files {
		broken += r.Broken
	}
	if broken == 0 {
		t.Fatal("no malformed lines generated; parser damage handling would be untested")
	}
}

func TestMalformZeroProducesCleanOutput(t *testing.T) {
	c := defaults()
	c.malform = 0
	m := readManifest(t, gen(t, c))

	for _, r := range m.Files {
		if r.Broken != 0 {
			t.Errorf("%s: %d broken lines with -malform=0", r.File, r.Broken)
		}
	}
}

// Rotated files exercise directory walking, chronological ordering, and
// transparent gzip decompression.
func TestRotatedFilesAreWritten(t *testing.T) {
	dir := gen(t, defaults())

	for _, name := range []string{"access.log.1", "access.log.2.gz"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("stat %s: %v", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty", name)
		}
	}

	c := defaults()
	c.rotate = false
	noRotate := gen(t, c)
	if _, err := os.Stat(filepath.Join(noRotate, "access.log.1")); !os.IsNotExist(err) {
		t.Error("access.log.1 written despite -rotate=false")
	}
}

// Fixtures must not age. Timestamps are anchored to a fixed end instant so that
// golden files stay valid indefinitely.
func TestTimestampsAreAnchoredNotWallClock(t *testing.T) {
	dir := gen(t, defaults())
	b, err := os.ReadFile(filepath.Join(dir, "checkout-api.log"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(b), "2026-08-13T14:") {
		t.Error("expected timestamps anchored to 2026-08-13; generator may be using wall clock")
	}
}

func TestScenarios(t *testing.T) {
	for _, scenario := range []string{"steady", "incident", "deploy-regression", "quiet"} {
		t.Run(scenario, func(t *testing.T) {
			c := defaults()
			c.scenario = scenario
			m := readManifest(t, gen(t, c))
			if len(m.Files) != len(sinks) {
				t.Errorf("got %d files, want %d", len(m.Files), len(sinks))
			}
			if m.Scenario != scenario {
				t.Errorf("manifest scenario = %q, want %q", m.Scenario, scenario)
			}
		})
	}
}

// The incident scenario's value is the root cause landing before the symptoms.
// Dragging the timeline back from the error spike to find it is the demo, and
// it only works if these lines are present and ordered correctly.
func TestIncidentScenarioHasRootCause(t *testing.T) {
	dir := gen(t, defaults())
	b, err := os.ReadFile(filepath.Join(dir, "postgresql.log"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{
		"connection pool exhausted",
		"remaining connection slots are reserved for superusers",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("root cause line missing: %q", want)
		}
	}
}
