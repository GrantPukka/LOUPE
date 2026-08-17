package main

import (
	"os"
	"path/filepath"
	"testing"
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
