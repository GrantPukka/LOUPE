package parse

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/quick"
)

// A cheap gate in front of an expensive expression is only sound if it rejects
// nothing the expression would have accepted. Reject one line too many and the
// parser stops claiming it, another parser claims it instead, and a record
// quietly changes format — which is exactly how a pg_severity:FATAL went
// missing when detection was last made faster.
//
// So each gate is checked against its own expression over every line of every
// fixture in the tree, and then over random input.
var gates = []struct {
	name string
	gate func([]byte) bool
	re   *regexp.Regexp
}{
	{"nginx", couldBeNginx, nginxRe},
	{"log4j", couldBeLog4j, log4jRe},
	{"haproxy", couldBeHAProxy, haproxyRe},
}

func TestDetectionGatesRejectOnlyWhatTheExpressionRejects(t *testing.T) {
	lines := everyFixtureLine(t)
	if len(lines) < 100 {
		t.Fatalf("only %d fixture lines; this test needs the corpus to mean anything", len(lines))
	}

	for _, g := range gates {
		var gated int
		for _, line := range lines {
			if g.gate(line) {
				continue
			}
			gated++
			if g.re.Match(line) {
				t.Errorf("%s gate rejected a line its expression accepts:\n  %q", g.name, line)
			}
		}
		t.Logf("%s: gate skipped the expression on %d of %d lines", g.name, gated, len(lines))
	}
}

// The fixtures are real formats but a finite set. Random input covers the
// shapes nobody thought to write down.
func TestDetectionGatesAreConservativeOnRandomInput(t *testing.T) {
	for _, g := range gates {
		g := g
		check := func(s string) bool {
			line := []byte(s)
			return g.gate(line) || !g.re.Match(line)
		}
		if err := quick.Check(check, &quick.Config{MaxCount: 20000}); err != nil {
			t.Errorf("%s gate is not conservative: %v", g.name, err)
		}
	}
}

// everyFixtureLine reads every line of every golden fixture under testdata.
func everyFixtureLine(t *testing.T) [][]byte {
	t.Helper()

	var out [][]byte
	err := filepath.WalkDir("testdata", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".log") {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)
		for sc.Scan() {
			out = append(out, append([]byte(nil), sc.Bytes()...))
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	return out
}

// everyFixtureFile lists the golden fixture logs under testdata.
func everyFixtureFile(t *testing.T) []string {
	t.Helper()

	var out []string
	err := filepath.WalkDir("testdata", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".log") {
			return err
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk fixtures: %v", err)
	}
	return out
}
