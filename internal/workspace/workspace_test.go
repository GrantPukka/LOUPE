package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newWorkspace(t *testing.T) *Workspace {
	t.Helper()

	w, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return w
}

func logDir(t *testing.T, name string) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.log"), []byte("a line\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return dir
}

func TestSubscribeAndList(t *testing.T) {
	w := newWorkspace(t)
	dir := logDir(t, "logs")

	sub, err := w.Subscribe(dir, "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if !sub.Active {
		t.Error("a new subscription is not active")
	}
	if sub.Name() != "logs" {
		t.Errorf("Name() = %q, want the directory's base name", sub.Name())
	}

	if paths := w.ActivePaths(); len(paths) != 1 {
		t.Errorf("ActivePaths() = %v, want one entry", paths)
	}
}

// The list must survive the process, or a subscription is just a session
// setting with extra steps.
func TestSubscriptionsPersist(t *testing.T) {
	dir := t.TempDir()
	logs := logDir(t, "logs")

	first, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := first.Subscribe(logs, "incident"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	second, err := Load(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	active := second.Active()
	if len(active) != 1 {
		t.Fatalf("got %d subscriptions after reload, want 1", len(active))
	}
	if active[0].Label != "incident" {
		t.Errorf("Label = %q, want the one that was saved", active[0].Label)
	}
}

// Unsubscribing keeps the entry and its history, so re-subscribing during an
// incident is instant rather than a re-read.
func TestUnsubscribeKeepsTheEntry(t *testing.T) {
	w := newWorkspace(t)
	dir := logDir(t, "logs")

	if _, err := w.Subscribe(dir, ""); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := w.Unsubscribe(dir); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}

	if len(w.ActivePaths()) != 0 {
		t.Error("an unsubscribed location is still active")
	}
	if len(w.All()) != 1 {
		t.Error("unsubscribing removed the entry; it should be kept, marked inactive")
	}

	sub, found := w.Find(dir)
	if !found {
		t.Fatal("the entry cannot be found after unsubscribing")
	}
	if sub.RemovedAt == nil {
		t.Error("no removal time recorded")
	}
}

// Re-subscribing keeps the original AddedAt, so the trail shows how long a
// location has been known rather than resetting each time.
func TestResubscribeKeepsTheOriginalDate(t *testing.T) {
	w := newWorkspace(t)
	dir := logDir(t, "logs")

	first, _ := w.Subscribe(dir, "")
	w.Unsubscribe(dir)
	again, err := w.Subscribe(dir, "")
	if err != nil {
		t.Fatalf("resubscribe: %v", err)
	}

	if !again.AddedAt.Equal(first.AddedAt) {
		t.Errorf("AddedAt changed from %v to %v on resubscribe", first.AddedAt, again.AddedAt)
	}
	if again.RemovedAt != nil {
		t.Error("the removal time survived a resubscribe")
	}
	if len(w.All()) != 1 {
		t.Errorf("resubscribing produced %d entries, want 1", len(w.All()))
	}
}

// Two routes to the same directory are one subscription, or the same files get
// ingested twice.
func TestPathsAreCanonicalised(t *testing.T) {
	w := newWorkspace(t)
	dir := logDir(t, "logs")

	if _, err := w.Subscribe(dir, ""); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := w.Subscribe(dir+"/.", ""); err != nil {
		t.Fatalf("Subscribe again: %v", err)
	}
	if _, err := w.Subscribe(filepath.Join(dir, "..", filepath.Base(dir)), ""); err != nil {
		t.Fatalf("Subscribe via parent: %v", err)
	}

	if len(w.All()) != 1 {
		t.Errorf("three routes to one directory produced %d subscriptions", len(w.All()))
	}
}

// Subscribing to something unreadable would produce a list entry that silently
// never works.
func TestSubscribeRejectsAMissingPath(t *testing.T) {
	w := newWorkspace(t)

	if _, err := w.Subscribe(filepath.Join(t.TempDir(), "nope"), ""); err == nil {
		t.Fatal("subscribing to a missing path succeeded")
	}
	if len(w.All()) != 0 {
		t.Error("a failed subscription was still added to the list")
	}

	// The attempt is still recorded: the trail is what happened, including
	// what did not work.
	events, err := w.Audit(0)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(events) == 0 || !strings.Contains(events[len(events)-1].Action, "failed") {
		t.Error("a failed subscription was not recorded in the audit trail")
	}
}

// The list is state; the trail is what happened. Deleting from one must not
// rewrite the other.
func TestForgetLeavesTheAuditTrail(t *testing.T) {
	w := newWorkspace(t)
	dir := logDir(t, "logs")

	w.Subscribe(dir, "")
	w.Unsubscribe(dir)
	if err := w.Forget(dir); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	if len(w.All()) != 0 {
		t.Error("the entry survived Forget")
	}

	events, err := w.Audit(0)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}

	var actions []string
	for _, e := range events {
		actions = append(actions, e.Action)
	}
	for _, want := range []string{"subscribe", "unsubscribe", "forget"} {
		found := false
		for _, a := range actions {
			if a == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the trail is missing %q: %v", want, actions)
		}
	}
}

func TestAuditIsAppendOnly(t *testing.T) {
	w := newWorkspace(t)
	a, b := logDir(t, "a"), logDir(t, "b")

	w.Subscribe(a, "")
	w.Subscribe(b, "")
	w.Unsubscribe(a)

	events, err := w.Audit(0)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}

	// Oldest first, and every entry stamped and attributed.
	for i, e := range events {
		if e.At.IsZero() {
			t.Errorf("event %d has no timestamp", i)
		}
		if i > 0 && e.At.Before(events[i-1].At) {
			t.Errorf("event %d is out of order", i)
		}
	}
}

func TestUnsubscribeUnknownPath(t *testing.T) {
	w := newWorkspace(t)

	if err := w.Unsubscribe(t.TempDir()); err == nil {
		t.Error("unsubscribing from something never subscribed succeeded")
	}
}

// A corrupt list must not stop the tool running.
func TestCorruptStateDoesNotBlockStartup(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	w, err := Load(dir)
	if err == nil {
		t.Error("a corrupt list loaded without complaint")
	}
	if w == nil {
		t.Fatal("Load returned nothing; the tool would refuse to start")
	}
	if len(w.All()) != 0 {
		t.Error("a corrupt list produced subscriptions")
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}

	got, err := ExpandHome("~/logs")
	if err != nil {
		t.Fatalf("ExpandHome: %v", err)
	}
	if got != filepath.Join(home, "logs") {
		t.Errorf("ExpandHome(~/logs) = %q", got)
	}

	if got, _ := ExpandHome("/absolute"); got != "/absolute" {
		t.Errorf("an absolute path was rewritten to %q", got)
	}
}

// Browsing is confined to the roots. Even if the Host check were defeated, an
// attacker reaches a bounded set of directories rather than the filesystem.
func TestBrowseRefusesOutsideTheRoots(t *testing.T) {
	w := newWorkspace(t)

	// Nothing subscribed, so a random temp directory is outside every root.
	outside := t.TempDir()
	if _, err := w.Browse(outside); err == nil {
		t.Fatal("browsed a directory outside every root")
	} else if !strings.Contains(err.Error(), "outside") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
}

func TestBrowseInsideASubscribedRoot(t *testing.T) {
	w := newWorkspace(t)
	dir := logDir(t, "logs")

	if _, err := w.Subscribe(dir, ""); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	listing, err := w.Browse(dir)
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}

	if !listing.Subscribed {
		t.Error("the listed directory is not marked as subscribed")
	}
	if listing.LogCount == 0 {
		t.Error("app.log was not counted as a log")
	}

	var found bool
	for _, e := range listing.Entries {
		if e.Name == "app.log" {
			found = true
			if !e.LooksLikeLog {
				t.Error("app.log is not marked as a log")
			}
		}
	}
	if !found {
		t.Error("app.log is missing from the listing")
	}
}

// An empty path returns the roots, so a fresh UI has somewhere to start.
func TestBrowseWithNoPathReturnsRoots(t *testing.T) {
	w := newWorkspace(t)

	listing, err := w.Browse("")
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if len(listing.Roots) == 0 {
		t.Error("no roots offered")
	}
	if listing.Path != "" {
		t.Errorf("Path = %q, want empty for the root listing", listing.Path)
	}
}

// Parent is empty at a root, which is what stops the UI offering to climb above
// one.
func TestBrowseStopsAtTheRoot(t *testing.T) {
	w := newWorkspace(t)
	dir := logDir(t, "logs")
	w.Subscribe(dir, "")

	listing, err := w.Browse(dir)
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}

	// The parent of a subscribed directory is also a root, so climbing once is
	// allowed; the point is that it stops somewhere.
	if listing.Parent != "" {
		above, err := w.Browse(listing.Parent)
		if err != nil {
			t.Fatalf("Browse parent: %v", err)
		}
		if above.Parent != "" {
			if _, err := w.Browse(filepath.Dir(above.Parent)); err == nil {
				t.Error("browsing climbed two levels above the deepest root")
			}
		}
	}
}

func TestLooksLikeLog(t *testing.T) {
	tests := map[string]bool{
		"app.log":      true,
		"access.log.1": true,
		"syslog":       true,
		"messages":     true,
		"out.jsonl":    true,
		"main.go":      false,
		"README.md":    false,
		"photo.png":    false,
	}

	for name, want := range tests {
		t.Run(name, func(t *testing.T) {
			if got := looksLikeLog(name, 100); got != want {
				t.Errorf("looksLikeLog(%q) = %v, want %v", name, got, want)
			}
		})
	}

	if looksLikeLog("app.log", 0) {
		t.Error("an empty file was marked as a log")
	}
}
