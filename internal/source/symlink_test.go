package source

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// podTree is the layout a Kubernetes node has: the real files under pods/, and
// containers/ full of symlinks into it. It is the layout every log-shipper
// document tells people to point a tool at.
func podTree(t *testing.T, pods int) string {
	t.Helper()
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "containers"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= pods; i++ {
		podDir := filepath.Join(dir, "pods", "app-"+strconv.Itoa(i))
		if err := os.MkdirAll(podDir, 0o755); err != nil {
			t.Fatal(err)
		}

		real := filepath.Join(podDir, "0.log")
		if err := os.WriteFile(real, []byte("2026-08-13T14:02:00Z stdout F hello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "containers", "app-"+strconv.Itoa(i)+"-abc.log")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	return dir
}

func walkNames(t *testing.T, root string, opts *WalkOptions) []string {
	t.Helper()

	got, err := Walk(root, opts)
	if err != nil {
		t.Fatalf("Walk(%s): %v", root, err)
	}

	out := make([]string, len(got))
	for i, s := range got {
		rel, err := filepath.Rel(root, s.Name())
		if err != nil {
			rel = s.Name()
		}
		out[i] = rel
	}
	sort.Strings(out)
	return out
}

// The case this exists for: a directory of symlinks reads, where before it was
// skipped in its entirety.
func TestWalkFollowsSymlinkedFiles(t *testing.T) {
	dir := podTree(t, 3)

	var opts WalkOptions
	got := walkNames(t, filepath.Join(dir, "containers"), &opts)

	want := []string{"app-1-abc.log", "app-2-abc.log", "app-3-abc.log"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("walked %v, want %v (skipped: %+v)", got, want, opts.Skipped)
	}
}

// The part that needed care. Both directories hold the same bytes, so walking
// the parent must read each record once — not twice, silently.
func TestWalkDeduplicatesTheSameFileUnderTwoNames(t *testing.T) {
	dir := podTree(t, 3)

	var opts WalkOptions
	got := walkNames(t, dir, &opts)

	if len(got) != 3 {
		t.Fatalf("walked %d file(s), want 3: %v", len(got), got)
	}

	// The real file wins, so the name reported is where the bytes actually
	// live rather than a link that may be removed while the file remains.
	for _, name := range got {
		if !strings.HasPrefix(name, "pods/") {
			t.Errorf("kept %q, want the real file under pods/", name)
		}
	}

	// Never silently: what was passed over is counted and named.
	var duplicates int
	for _, skip := range opts.Skipped {
		if skip.Reason == duplicateReason {
			duplicates++
		}
	}
	if duplicates != 3 {
		t.Errorf("%d duplicate(s) reported, want 3: %+v", duplicates, opts.Skipped)
	}
}

// Whatever order the walk finds them in, the same file is kept.
func TestWalkDeduplicationIsDeterministic(t *testing.T) {
	dir := podTree(t, 4)

	first := walkNames(t, dir, &WalkOptions{})
	for i := 0; i < 5; i++ {
		if got := walkNames(t, dir, &WalkOptions{}); strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("run %d walked %v, first run walked %v", i, got, first)
		}
	}
}

// A directory reached through a symlink is a directory. filepath.WalkDir lstats
// its root, so without resolving it first the whole tree is skipped as "not a
// regular file" — which is what `loupe /var/log/mylogs` used to do.
func TestWalkResolvesASymlinkedRoot(t *testing.T) {
	dir := podTree(t, 2)

	link := filepath.Join(t.TempDir(), "logs")
	if err := os.Symlink(filepath.Join(dir, "pods"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var opts WalkOptions
	got, err := Walk(link, &opts)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("walked %d file(s) through a symlinked root, want 2 (skipped: %+v)",
			len(got), opts.Skipped)
	}
}

// A symlink to a directory *inside* a walk is not followed: it can point at its
// own ancestor and there is no cheap way to know it does not. The reason says
// what does work.
func TestWalkDoesNotFollowNestedDirectorySymlinks(t *testing.T) {
	dir := podTree(t, 1)

	if err := os.Symlink(filepath.Join(dir, "pods"), filepath.Join(dir, "loop")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var opts WalkOptions
	got := walkNames(t, dir, &opts)

	if len(got) != 1 {
		t.Errorf("walked %d file(s), want 1: %v", len(got), got)
	}

	var named bool
	for _, skip := range opts.Skipped {
		if strings.HasSuffix(skip.Path, "loop") {
			named = true
			if !strings.Contains(skip.Reason, "name it directly") {
				t.Errorf("the reason does not say what works: %q", skip.Reason)
			}
		}
	}
	if !named {
		t.Errorf("the directory symlink was not reported: %+v", opts.Skipped)
	}
}

// A symlink whose target is gone is a note, not a failure. A container that
// exited between the directory listing and the stat leaves exactly that behind.
func TestWalkReportsBrokenSymlinks(t *testing.T) {
	dir := podTree(t, 1)

	broken := filepath.Join(dir, "containers", "gone.log")
	if err := os.Symlink(filepath.Join(dir, "nothing-here.log"), broken); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var opts WalkOptions
	got := walkNames(t, dir, &opts)

	if len(got) != 1 {
		t.Errorf("a broken symlink changed what was read: %v", got)
	}

	var found bool
	for _, skip := range opts.Skipped {
		if strings.HasSuffix(skip.Path, "gone.log") && skip.Reason == "broken symlink" {
			found = true
		}
	}
	if !found {
		t.Errorf("the broken symlink was not reported: %+v", opts.Skipped)
	}
}

// A symlink to a regular file is a regular file for reading. Nothing else is.
func TestWalkStillSkipsWhatIsNotAFile(t *testing.T) {
	dir := t.TempDir()

	fifo := filepath.Join(dir, "pipe")
	if err := makeFIFO(fifo); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}
	if err := os.Symlink(fifo, filepath.Join(dir, "pipe-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real.log"), []byte("a line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var opts WalkOptions
	got := walkNames(t, dir, &opts)

	if strings.Join(got, ",") != "real.log" {
		t.Errorf("walked %v, want only real.log", got)
	}

	reasons := map[string]string{}
	for _, skip := range opts.Skipped {
		reasons[filepath.Base(skip.Path)] = skip.Reason
	}
	if reasons["pipe"] != "not a regular file" {
		t.Errorf("a FIFO was reported as %q", reasons["pipe"])
	}
	if !strings.Contains(reasons["pipe-link"], "not a regular file") {
		t.Errorf("a symlink to a FIFO was reported as %q", reasons["pipe-link"])
	}
}

// Naming one symlinked file works too. Walk stats the root, which follows, so
// this path never reached the directory logic — but it is what someone types
// after `ls /var/log/containers`, so it is worth pinning.
func TestWalkAcceptsASingleSymlinkedFile(t *testing.T) {
	dir := podTree(t, 1)
	link := filepath.Join(dir, "containers", "app-1-abc.log")

	got, err := Walk(link, &WalkOptions{})
	if err != nil {
		t.Fatalf("Walk(%s): %v", link, err)
	}
	if len(got) != 1 {
		t.Fatalf("walked %d source(s), want 1", len(got))
	}

	// The name is the one that was asked for. Nothing resolved it, because
	// nothing needed to.
	if got[0].Name() != link {
		t.Errorf("named %q, want %q", got[0].Name(), link)
	}
}
