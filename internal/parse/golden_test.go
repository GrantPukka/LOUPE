package parse

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// update regenerates the golden files instead of comparing against them:
//
//	go test ./internal/parse -run TestParsers -update
//
// Always read `git diff testdata/` afterwards. The golden file is the actual
// test; regenerating without reading the diff turns a failing test green
// without anyone deciding that the new output is correct.
var update = flag.Bool("update", false, "regenerate golden files")

// goldenRecord is the on-disk shape. It is deliberately verbose and stable
// rather than compact: a golden file is read by humans during review, and a
// diff has to make it obvious what changed.
type goldenRecord struct {
	LineNo    int64          `json:"line_no"`
	Timestamp string         `json:"ts,omitempty"`
	Zoned     bool           `json:"ts_zoned,omitempty"`
	Level     string         `json:"level,omitempty"`
	Message   string         `json:"message,omitempty"`
	Fields    map[string]any `json:"fields,omitempty"`
	Parsed    bool           `json:"parsed"`
	Truncated bool           `json:"truncated,omitempty"`
	Raw       string         `json:"raw"`
}

type goldenFile struct {
	Parser  string         `json:"parser"`
	Stats   Stats          `json:"stats"`
	Records []goldenRecord `json:"records"`
}

// TestParsers runs every fixture directory under testdata through its parser
// and compares the result against the checked-in golden file.
//
// Fixture layout, one directory per format:
//
//	testdata/<format>/sample.log     the input
//	testdata/<format>/sample.golden  the expected records
//
// The directory name is the parser name, so adding a format means adding a
// directory and running with -update. No test code changes.
func TestParsers(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("testdata", "*"))
	if err != nil {
		t.Fatalf("glob testdata: %v", err)
	}
	if len(dirs) == 0 {
		t.Skip("no fixtures in testdata/ yet")
	}

	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}

		format := filepath.Base(dir)
		t.Run(format, func(t *testing.T) {
			p, ok := Get(format)
			if !ok {
				t.Fatalf("fixture directory testdata/%s has no matching parser; "+
					"the directory name must be the parser name", format)
			}

			samples, err := filepath.Glob(filepath.Join(dir, "*.log"))
			if err != nil {
				t.Fatalf("glob: %v", err)
			}
			if len(samples) == 0 {
				t.Fatalf("testdata/%s contains no *.log fixture", format)
			}

			for _, sample := range samples {
				t.Run(filepath.Base(sample), func(t *testing.T) {
					checkGolden(t, p, sample)
				})
			}
		})
	}
}

func checkGolden(t *testing.T, p Parser, samplePath string) {
	t.Helper()

	input, err := os.Open(samplePath)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer input.Close()

	var records []goldenRecord
	stats, _, err := ReadAll(input, ReaderOptions{Parser: p, Loc: time.UTC}, func(e Entry) error {
		records = append(records, toGolden(e))
		return nil
	})
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	got := goldenFile{Parser: p.Name(), Stats: stats, Records: records}
	encoded, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	encoded = append(encoded, '\n')

	goldenPath := strings.TrimSuffix(samplePath, filepath.Ext(samplePath)) + ".golden"

	if *update {
		if err := os.WriteFile(goldenPath, encoded, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote %s (%d records) — read the diff before committing", goldenPath, len(records))
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}

	if string(encoded) != string(want) {
		t.Errorf("output differs from %s\n%s", goldenPath, firstDifference(string(want), string(encoded)))
	}
}

func toGolden(e Entry) goldenRecord {
	g := goldenRecord{
		LineNo:    e.LineNo,
		Zoned:     e.TimestampZoned,
		Level:     e.Level,
		Message:   e.Message,
		Parsed:    e.Parsed,
		Truncated: e.Truncated,
		Raw:       e.Raw,
	}
	if e.HasTimestamp() {
		// UTC and a fixed layout, so golden files do not depend on the machine
		// running the test.
		g.Timestamp = e.Timestamp.UTC().Format(time.RFC3339Nano)
	}
	if len(e.Fields) > 0 {
		g.Fields = e.Fields
	}
	return g
}

// firstDifference reports the first differing line with a little context. A
// full diff of a 500-record golden file is unreadable in test output.
func firstDifference(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")

	for i := 0; i < len(wantLines) && i < len(gotLines); i++ {
		if wantLines[i] == gotLines[i] {
			continue
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "first difference at line %d:\n", i+1)
		for j := max(0, i-2); j < min(len(wantLines), i+3); j++ {
			marker := "  "
			if j == i {
				marker = "- "
			}
			fmt.Fprintf(&sb, "%s%s\n", marker, wantLines[j])
		}
		for j := max(0, i-2); j < min(len(gotLines), i+3); j++ {
			if j == i {
				fmt.Fprintf(&sb, "+ %s\n", gotLines[j])
			}
		}
		return sb.String()
	}

	return fmt.Sprintf("golden has %d lines, got %d", len(wantLines), len(gotLines))
}

// TestFixturesAreMessy enforces the CONTRIBUTING.md rule that fixtures contain
// real-world damage.
//
// A parser tested only against clean input is tested against the case that was
// never going to fail. This is the check that keeps the never-lose-data
// principle honest as parsers get added.
func TestFixturesAreMessy(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("testdata", "*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(dirs) == 0 {
		t.Skip("no fixtures yet")
	}

	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}

		t.Run(filepath.Base(dir), func(t *testing.T) {
			goldens, err := filepath.Glob(filepath.Join(dir, "*.golden"))
			if err != nil || len(goldens) == 0 {
				t.Skip("no golden file yet")
			}

			var total, unparsed int
			for _, path := range goldens {
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read %s: %v", path, err)
				}
				var g goldenFile
				if err := json.Unmarshal(b, &g); err != nil {
					t.Fatalf("unmarshal %s: %v", path, err)
				}
				total += len(g.Records)
				unparsed += int(g.Stats.Unparsed)
			}

			if total < 20 {
				t.Errorf("only %d records across the fixtures; CONTRIBUTING.md asks for 20-50", total)
			}
			if unparsed == 0 {
				t.Error("no malformed records in the fixtures: " +
					"add a truncated line and at least one broken record, " +
					"or damage handling is untested for this format")
			}
		})
	}
}

// Fixtures must stay small enough to review in a pull request.
func TestFixturesAreSmall(t *testing.T) {
	const maxBytes = 1 << 20 // CLAUDE.md: never commit fixtures over 1MB

	err := filepath.Walk("testdata", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if info.Size() > maxBytes {
			t.Errorf("%s is %dKB; fixtures over 1MB must not be committed", path, info.Size()/1024)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk testdata: %v", err)
	}
}
