package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// following returns a model that has run its initial query and has a
// follower attached, plus the directory to append to.
func following(t *testing.T, filter string) (model, string) {
	t.Helper()

	sess, dir := openFixtureDir(t)
	m := newModel(context.Background(), sess, filter)
	m.width, m.height = 120, 30

	follower, err := sess.Follower(context.Background())
	if err != nil {
		t.Fatalf("Follower: %v", err)
	}
	m.follower = follower

	// Run the initial query the way the runtime would, without going through
	// Init: with a follower attached Init batches the query with a tick, and
	// a BatchMsg is the runtime's business rather than the model's.
	msg := m.runQuery(filter)()
	if e, ok := msg.(errMsg); ok {
		t.Fatalf("initial query failed: %v", e.err)
	}
	next, _ := m.Update(msg)

	return next.(model), dir
}

func appendLines(t *testing.T, dir string, lines ...string) {
	t.Helper()

	f, err := os.OpenFile(filepath.Join(dir, "app.log"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	defer f.Close()

	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
}

// pollOnce runs one poll and feeds the result back, as the runtime would.
func pollOnce(t *testing.T, m model) model {
	t.Helper()

	msg, ok := m.poll()().(liveMsg)
	if !ok {
		t.Fatal("a poll did not return a liveMsg")
	}
	next, _ := m.Update(msg)
	return next.(model)
}

// A quiet directory must not change anything. Following something nobody is
// writing to should be indistinguishable from not following it.
func TestFollowPollWithNothingWrittenChangesNothing(t *testing.T) {
	m, _ := following(t, "")
	before := len(m.rows)

	m = pollOnce(t, m)

	if len(m.rows) != before {
		t.Errorf("a quiet poll changed the row count from %d to %d", before, len(m.rows))
	}
	if m.unseen != 0 {
		t.Errorf("a quiet poll reported %d unseen records", m.unseen)
	}
}

// The point of --follow: a line written afterwards is appended, and the cursor
// stays on it because it was already at the end.
func TestFollowAppendsAndKeepsTheCursorAtTheEnd(t *testing.T) {
	m, dir := following(t, "")

	// The list is oldest-first, so the end is where new records land.
	m.cursor = len(m.rows) - 1
	before := len(m.rows)

	appendLines(t, dir,
		`{"ts":"2026-08-13T14:04:00Z","level":"error","msg":"live one","status":503}`)

	m = pollOnce(t, m)

	if len(m.rows) != before+1 {
		t.Fatalf("rows = %d, want %d", len(m.rows), before+1)
	}
	if m.cursor != len(m.rows)-1 {
		t.Errorf("cursor = %d, want %d — it should follow the tail down",
			m.cursor, len(m.rows)-1)
	}
	if m.unseen != 0 {
		t.Errorf("unseen = %d; a cursor riding the end has nothing unread", m.unseen)
	}
	if got := m.rows[len(m.rows)-1][m.columns["message"]]; got != "live one" {
		t.Errorf("last row = %v, want the appended record", got)
	}
}

// Somebody reading further up must not be dragged to the bottom. During an
// incident that is constant, and it makes the view unusable.
func TestFollowDoesNotMoveACursorThatIsReadingElsewhere(t *testing.T) {
	m, dir := following(t, "")
	m.cursor = 0

	appendLines(t, dir,
		`{"ts":"2026-08-13T14:05:00Z","level":"error","msg":"live two","status":503}`,
		`{"ts":"2026-08-13T14:05:01Z","level":"warn","msg":"live three","status":429}`)

	m = pollOnce(t, m)

	if m.cursor != 0 {
		t.Errorf("cursor moved to %d while the reader was at the top", m.cursor)
	}
	// Not silently: the count is what tells them the tail is still running.
	if m.unseen != 2 {
		t.Errorf("unseen = %d, want 2", m.unseen)
	}
	if !strings.Contains(m.footer(), "2 new below") {
		t.Errorf("footer does not report the unseen records:\n%s", m.footer())
	}

	// G is the way to catch up, and catching up clears the count.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	m = next.(model)
	if m.unseen != 0 {
		t.Errorf("unseen = %d after G, want 0", m.unseen)
	}
}

// A live record must go through the same compiled filter as everything else. A
// tail that showed what the filter excludes would disagree with itself.
func TestFollowAppliesTheFilter(t *testing.T) {
	m, dir := following(t, "level:error")
	before := len(m.rows)

	appendLines(t, dir,
		`{"ts":"2026-08-13T14:06:00Z","level":"info","msg":"ignored","status":200}`,
		`{"ts":"2026-08-13T14:06:01Z","level":"error","msg":"kept","status":500}`)

	m = pollOnce(t, m)

	if len(m.rows) != before+1 {
		t.Fatalf("rows = %d, want %d — only the error should have been added",
			len(m.rows), before+1)
	}
	if got := m.rows[len(m.rows)-1][m.columns["message"]]; got != "kept" {
		t.Errorf("last row = %v, want the matching record", got)
	}
}

// A batch fetched under a filter the user has since changed is discarded. The
// records are in the store, so the new filter's own query finds them; folding
// them in would show records the current filter excludes.
func TestFollowDiscardsABatchForAStaleFilter(t *testing.T) {
	m, _ := following(t, "")
	before := len(m.rows)

	next, _ := m.Update(liveMsg{
		filter: "level:error",
		rows:   [][]any{make([]any, len(m.columns))},
	})
	m = next.(model)

	if len(m.rows) != before {
		t.Errorf("rows = %d, want %d — a stale batch was folded in", len(m.rows), before)
	}
}

// A source that stopped being readable is reported, not swallowed, and not
// repeated on every poll until it is the only thing on the footer.
func TestFollowReportsNoticesOnce(t *testing.T) {
	m, _ := following(t, "")

	for i := 0; i < 3; i++ {
		next, _ := m.Update(liveMsg{filter: m.applied, notices: []string{"read auth.log: permission denied"}})
		m = next.(model)
	}

	if len(m.notices) != 1 {
		t.Errorf("notices = %v, want one copy of the warning", m.notices)
	}
	if !strings.Contains(m.footer(), "permission denied") {
		t.Errorf("footer does not carry the warning:\n%s", m.footer())
	}
}

// Without --follow there is no follower, nothing polls, and the footer says
// nothing about following.
func TestWithoutFollowNothingIsFollowed(t *testing.T) {
	m := loaded(t, "")

	if m.follower != nil {
		t.Error("a model built without --follow has a follower")
	}
	if strings.Contains(m.footer(), "following") {
		t.Errorf("footer claims to be following:\n%s", m.footer())
	}
}
