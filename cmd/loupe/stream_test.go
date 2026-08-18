package main

import (
	"strings"
	"testing"

	"github.com/GrantPukka/loupe/internal/session"
)

// A bare dash is standard input, not a search term for the word "-".
func TestResolveArgsTreatsADashAsStdin(t *testing.T) {
	paths, filter, err := resolveArgs([]string{"-"})
	if err != nil {
		t.Fatalf("resolveArgs: %v", err)
	}
	if len(paths) != 1 || paths[0] != session.StdinPath {
		t.Errorf("paths = %v, want [%q]", paths, session.StdinPath)
	}
	if filter != "" {
		t.Errorf("filter = %q, want empty", filter)
	}
}

// It composes with real paths, in either order, which is what makes reading a
// directory and a pipe on one timeline possible.
func TestResolveArgsComposesStdinWithPathsAndFilters(t *testing.T) {
	dir := t.TempDir()

	paths, filter, err := resolveArgs([]string{dir, "-", "level:error"})
	if err != nil {
		t.Fatalf("resolveArgs: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("paths = %v, want the directory and stdin", paths)
	}
	if paths[1] != session.StdinPath {
		t.Errorf("paths = %v, want stdin second", paths)
	}
	if filter != "level:error" {
		t.Errorf("filter = %q, want %q", filter, "level:error")
	}
}

// A dash inside a longer argument is not stdin; only the bare one is.
func TestResolveArgsDoesNotTreatDashedTermsAsStdin(t *testing.T) {
	paths, filter, err := resolveArgs([]string{"-source:nginx"})
	if err != nil {
		t.Fatalf("resolveArgs: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("paths = %v, want none", paths)
	}
	if filter != "-source:nginx" {
		t.Errorf("filter = %q, want the exclusion term", filter)
	}
}

// A handoff needs a finished read to describe, and a stream has no end. Saying
// so beats writing an extract that silently covers whatever had arrived.
func TestStreamRefusesAHandoff(t *testing.T) {
	g := &globals{handoff: "out.md"}
	sess := &session.Session{}

	err := runStream(nil, g, sess, "")
	if err == nil {
		t.Fatal("--handoff on a stream was accepted")
	}
	if !strings.Contains(err.Error(), "no end") {
		t.Errorf("error does not explain why: %v", err)
	}
}
