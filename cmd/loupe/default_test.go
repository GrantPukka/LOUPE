package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GrantPukka/loupe/internal/source"
)

// A path-shaped argument that is not on disk must stop the run. The failure it
// prevents is silent: without it the argument is demoted to a filter term, no
// path is left, and the run falls back to the subscribed locations — reporting
// confident results about data the user never asked for.
func TestResolveArgsRejectsAMissingPath(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "logs")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		args    []string
		paths   []string
		filter  string
		wantErr bool
	}{
		{
			name:  "a directory that exists",
			args:  []string{existing},
			paths: []string{existing},
		},
		{
			name:   "a directory that exists, and a filter",
			args:   []string{existing, "level:error"},
			paths:  []string{existing},
			filter: "level:error",
		},
		{
			name:    "an absolute path that does not exist",
			args:    []string{"/nonexistent/path/xyz"},
			wantErr: true,
		},
		{
			name:    "a relative path that does not exist",
			args:    []string{"./logz"},
			wantErr: true,
		},
		{
			name:    "a bare directory name with a separator",
			args:    []string{"var/log/missing"},
			wantErr: true,
		},
		{
			name:    "a missing path alongside a real one",
			args:    []string{existing, "/nonexistent/xyz"},
			wantErr: true,
		},
		// The terms below all contain characters a path might, and must stay
		// filters. Misclassifying one turns a working query into an error.
		{
			name:   "a field term whose value is a path",
			args:   []string{"path:/api/checkout"},
			filter: "path:/api/checkout",
		},
		{
			name:   "a negated field term",
			args:   []string{"-source:nginx"},
			filter: "-source:nginx",
		},
		{
			name:   "a time window",
			args:   []string{"14:00-15:00"},
			filter: "14:00-15:00",
		},
		{
			name:   "a phrase containing a slash",
			args:   []string{"GET /api/orders"},
			filter: "GET /api/orders",
		},
		{
			name:   "a bare word",
			args:   []string{"timeout"},
			filter: "timeout",
		},
		{
			name:   "several filter terms join up",
			args:   []string{"level:error", "status:>=500"},
			filter: "level:error status:>=500",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			paths, filter, err := resolveArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveArgs(%q) succeeded, want an error", tt.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveArgs(%q): %v", tt.args, err)
			}
			if filter != tt.filter {
				t.Errorf("filter = %q, want %q", filter, tt.filter)
			}
			if len(paths) != len(tt.paths) {
				t.Fatalf("paths = %q, want %q", paths, tt.paths)
			}
			for i := range paths {
				if paths[i] != tt.paths[i] {
					t.Errorf("paths[%d] = %q, want %q", i, paths[i], tt.paths[i])
				}
			}
		})
	}
}

// The walk reports every file it passed over. Following symlinks made that a
// problem: a node walking /var/log finds every pod log twice, and three hundred
// lines saying so bury the counts the status line exists for.
func TestWriteSkipsCollapsesRepetition(t *testing.T) {
	many := make([]source.Skip, 0, 12)
	for i := 0; i < 12; i++ {
		many = append(many, source.Skip{
			Path:   fmt.Sprintf("/var/log/containers/app-%d.log", i),
			Reason: "already read under another name",
		})
	}
	many = append(many,
		source.Skip{Path: "/var/log/socket", Reason: "not a regular file"},
		source.Skip{Path: "/var/log/gone.log", Reason: "broken symlink"},
	)

	var buf bytes.Buffer
	writeSkips(&buf, many)
	got := buf.String()

	// The repeated reason is counted, once.
	if !strings.Contains(got, "Skipped 12 file(s): already read under another name") {
		t.Errorf("the repeated reason was not collapsed:\n%s", got)
	}
	if strings.Contains(got, "app-3.log") {
		t.Errorf("a collapsed group still listed its files:\n%s", got)
	}

	// The one-offs are still named, because that is what a reader wants when
	// there are only a few.
	for _, want := range []string{"/var/log/socket: not a regular file", "/var/log/gone.log: broken symlink"} {
		if !strings.Contains(got, want) {
			t.Errorf("a one-off skip was lost:\n%s", got)
		}
	}

	if lines := strings.Count(strings.TrimSpace(got), "\n") + 1; lines != 3 {
		t.Errorf("wrote %d line(s), want 3:\n%s", lines, got)
	}
}

// Below the threshold every file is named: a count says less than a path when
// there are two of them.
func TestWriteSkipsNamesTheFewByName(t *testing.T) {
	var buf bytes.Buffer
	writeSkips(&buf, []source.Skip{
		{Path: "a.png", Reason: "extension is not a log format"},
		{Path: "b.png", Reason: "extension is not a log format"},
	})

	got := buf.String()
	for _, want := range []string{"a.png", "b.png"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s was collapsed away:\n%s", want, got)
		}
	}
}

// Reported in walk order, so the output does not reorder itself between runs.
func TestWriteSkipsIsDeterministic(t *testing.T) {
	skips := []source.Skip{
		{Path: "z.log", Reason: "one"},
		{Path: "a.log", Reason: "two"},
		{Path: "m.log", Reason: "three"},
	}

	var first bytes.Buffer
	writeSkips(&first, skips)

	for i := 0; i < 5; i++ {
		var again bytes.Buffer
		writeSkips(&again, skips)
		if again.String() != first.String() {
			t.Fatalf("run %d differed:\n%s\n%s", i, first.String(), again.String())
		}
	}
}
