package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VIGIL-OPS/loupe/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

// openFixture builds a small session to drive the model against.
func openFixture(t *testing.T) *session.Session {
	t.Helper()
	dir := t.TempDir()

	lines := []string{
		`{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"started","status":200,"trace_id":"a1"}`,
		`{"ts":"2026-08-13T14:01:00Z","level":"error","msg":"upstream timeout","status":502,"trace_id":"a2"}`,
		`{"ts":"2026-08-13T14:02:00Z","level":"warn","msg":"rate limited","status":429}`,
		`{"ts":"2026-08-13T14:03:00Z","level":"error","msg":"boom\n\tat Foo.bar(Foo.java:1)","status":500}`,
		`{"level":"error","msg":"no timestamp here","status":500}`,
		`not json at all`,
	}
	if err := os.WriteFile(filepath.Join(dir, "app.log"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	sess, err := session.Open(context.Background(), session.Options{
		Path: dir, Location: time.UTC, NoCache: true,
	})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

// loaded returns a model that has run its initial query.
func loaded(t *testing.T, filter string) model {
	t.Helper()

	m := newModel(context.Background(), openFixture(t), filter)
	m.width, m.height = 120, 30

	// Init returns the query command; run it and feed the result back, which
	// is what the Bubble Tea runtime would do.
	msg := m.Init()()
	if e, ok := msg.(errMsg); ok {
		t.Fatalf("initial query failed: %v", e.err)
	}

	next, _ := m.Update(msg)
	return next.(model)
}

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// press sends a key and returns the new model plus any command it produced.
func press(m model, s string) (model, tea.Cmd) {
	next, cmd := m.Update(key(s))
	return next.(model), cmd
}

func TestInitialLoad(t *testing.T) {
	m := loaded(t, "")

	if m.total != 6 {
		t.Errorf("total = %d, want 6", m.total)
	}
	if len(m.rows) != 6 {
		t.Errorf("got %d rows, want 6", len(m.rows))
	}
	if m.err != nil {
		t.Errorf("err = %v", m.err)
	}
}

func TestViewRendersWithoutPanicking(t *testing.T) {
	m := loaded(t, "")

	view := m.View()
	if view == "" {
		t.Fatal("View returned nothing")
	}

	// The display timezone must always be on screen; a user who cannot tell
	// whose clock they are reading is the failure the spec guards against.
	if !strings.Contains(view, "UTC") {
		t.Error("the view does not state the display timezone")
	}
	for _, want := range []string{"loupe", "TIME", "LEVEL", "SOURCE", "MESSAGE"} {
		if !strings.Contains(view, want) {
			t.Errorf("the view is missing %q", want)
		}
	}
}

func TestFilteringThroughTheFilterBox(t *testing.T) {
	m := loaded(t, "")

	// / focuses the box, then the typed filter applies on enter.
	m, _ = press(m, "/")
	if m.focus != focusFilter {
		t.Fatal("/ did not focus the filter")
	}

	m.filter.SetValue("level:error")
	m, cmd := press(m, "enter")

	if m.focus != focusList {
		t.Error("enter did not return focus to the list")
	}
	if m.applied != "level:error" {
		t.Errorf("applied = %q", m.applied)
	}
	if cmd == nil {
		t.Fatal("enter did not run a query")
	}

	next, _ := m.Update(cmd())
	m = next.(model)

	if m.total != 3 {
		t.Errorf("total = %d, want the 3 error records", m.total)
	}
}

// Escape in the filter box abandons the edit rather than applying it, so a
// half-typed filter never silently changes what is on screen.
func TestEscapeAbandonsAFilterEdit(t *testing.T) {
	m := loaded(t, "level:error")

	m, _ = press(m, "/")
	m.filter.SetValue("level:nonsense")
	m, cmd := press(m, "esc")

	if cmd != nil {
		t.Error("escape ran a query")
	}
	if m.applied != "level:error" {
		t.Errorf("applied = %q, want the previous filter", m.applied)
	}
	if m.filter.Value() != "level:error" {
		t.Errorf("the box shows %q, want the applied filter restored", m.filter.Value())
	}
}

func TestEscapeFromTheListClearsTheFilter(t *testing.T) {
	m := loaded(t, "level:error")

	m, cmd := press(m, "esc")
	if m.applied != "" {
		t.Errorf("applied = %q, want empty", m.applied)
	}
	if cmd == nil {
		t.Fatal("clearing did not re-run the query")
	}
}

func TestMovement(t *testing.T) {
	m := loaded(t, "")

	m, _ = press(m, "j")
	if m.cursor != 1 {
		t.Errorf("cursor = %d after j, want 1", m.cursor)
	}

	m, _ = press(m, "k")
	if m.cursor != 0 {
		t.Errorf("cursor = %d after k, want 0", m.cursor)
	}

	// k at the top must not move above the first row.
	m, _ = press(m, "k")
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want it clamped at 0", m.cursor)
	}

	m, _ = press(m, "G")
	if m.cursor != len(m.rows)-1 {
		t.Errorf("cursor = %d after G, want the last row", m.cursor)
	}

	m, _ = press(m, "g")
	if m.cursor != 0 {
		t.Errorf("cursor = %d after g, want 0", m.cursor)
	}
}

func TestExpandARecord(t *testing.T) {
	m := loaded(t, "")

	m, cmd := press(m, "enter")
	if m.expanded < 0 {
		t.Fatal("enter did not expand the record")
	}
	if cmd == nil {
		t.Fatal("expanding did not fetch the detail")
	}

	next, _ := m.Update(cmd())
	m = next.(model)

	if m.detail == nil {
		t.Fatal("the detail never arrived")
	}

	// The raw line is always shown: a reader may not trust our parser.
	view := m.View()
	if !strings.Contains(view, "raw") {
		t.Error("the expanded record does not show its raw line")
	}

	// Enter again collapses.
	m, _ = press(m, "enter")
	if m.expanded >= 0 {
		t.Error("enter did not collapse the record")
	}
}

// An unparsed record must say so rather than looking like an ordinary one with
// missing fields.
func TestUnparsedRecordIsFlagged(t *testing.T) {
	m := loaded(t, "")

	// Walk to the unparsed line.
	for i := 0; i < len(m.rows); i++ {
		m.cursor = i
		m.expanded = -1

		next, cmd := m.Update(key("enter"))
		m = next.(model)
		if cmd == nil {
			continue
		}
		detail, _ := m.Update(cmd())
		m = detail.(model)

		if parsed, ok := m.detail["parsed"].(bool); ok && !parsed {
			if !strings.Contains(m.View(), "unparsed") {
				t.Error("an unparsed record is not marked in the detail pane")
			}
			return
		}
		m, _ = press(m, "enter")
	}
	t.Fatal("no unparsed record found in the fixture")
}

// f narrows to the selected row's source by writing a real DSL term into the
// filter box, the same principle as the web UI's click-to-filter.
func TestFilterBySourceWritesADSLTerm(t *testing.T) {
	m := loaded(t, "")

	m, cmd := press(m, "f")
	if cmd == nil {
		t.Fatal("f did not run a query")
	}
	if !strings.Contains(m.applied, "source:app") {
		t.Errorf("applied = %q, want it to contain source:app", m.applied)
	}
	if m.filter.Value() != m.applied {
		t.Errorf("the filter box shows %q but %q was applied; the term must be visible and editable",
			m.filter.Value(), m.applied)
	}
}

// A filter error shows its whole message, spelling suggestion included.
func TestFilterErrorIsShownInFull(t *testing.T) {
	m := loaded(t, "")

	m, _ = press(m, "/")
	m.filter.SetValue("sevrity:error")
	m, cmd := press(m, "enter")

	next, _ := m.Update(cmd())
	m = next.(model)

	if m.err == nil {
		t.Fatal("an unknown field did not produce an error")
	}

	view := m.View()
	for _, want := range []string{"unknown field", "fields present"} {
		if !strings.Contains(view, want) {
			t.Errorf("the view is missing %q from the error", want)
		}
	}
}

// A slow query for a filter the user has moved on from must not overwrite the
// current results.
func TestStaleResultsAreIgnored(t *testing.T) {
	m := loaded(t, "")
	before := len(m.rows)

	next, _ := m.Update(resultMsg{
		filter: "some other filter",
		rows:   nil,
		total:  0,
	})
	m = next.(model)

	if len(m.rows) != before {
		t.Errorf("a stale result overwrote the current rows (%d then %d)", before, len(m.rows))
	}
}

// The footer must declare records that could not be placed on the timeline.
func TestFooterDeclaresExcludedRecords(t *testing.T) {
	m := loaded(t, "")

	if m.hist.NoTimestamp == 0 {
		t.Skip("the fixture produced no untimestamped records")
	}
	if !strings.Contains(m.View(), "not on the timeline") {
		t.Error("the footer does not mention records missing from the timeline")
	}
}

// A bucket holding records is never drawn blank; a quiet period and a real
// cluster must not look the same.
func TestHistogramScaleNeverHidesRecords(t *testing.T) {
	if got := scale(0, 1000); got != 0 {
		t.Errorf("scale(0) = %d, want 0", got)
	}
	if got := scale(1, 1_000_000); got < 1 {
		t.Errorf("scale(1, 1000000) = %d; a bucket with records must render", got)
	}
	if got := scale(1000, 1000); got != 8 {
		t.Errorf("scale(max) = %d, want the full block", got)
	}
}

func TestSanitiseEscapesControlCharacters(t *testing.T) {
	got := sanitise("before\x00after\x1bmore")

	if strings.ContainsRune(got, 0) {
		t.Error("a NUL byte survived into the rendered line")
	}
	// The damage is evidence, so it is escaped rather than dropped.
	if !strings.Contains(got, `\0`) {
		t.Errorf("sanitise(%q) = %q, want the NUL shown", "before\x00after", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("hello world", 8); got != "hello w…" {
		t.Errorf("truncate = %q, want an ellipsis at 8", got)
	}
	if got := truncate("hello", 0); got != "" {
		t.Errorf("truncate to zero = %q", got)
	}
}

func TestCommas(t *testing.T) {
	tests := map[int64]string{0: "0", 42: "42", 1000: "1,000", 33939: "33,939", 1234567: "1,234,567"}
	for n, want := range tests {
		if got := commas(n); got != want {
			t.Errorf("commas(%d) = %q, want %q", n, got, want)
		}
	}
}

// The list must shrink to make room for an expanded record rather than
// overflowing the screen.
func TestListHeightAccountsForTheDetailPane(t *testing.T) {
	m := loaded(t, "")

	collapsed := m.listHeight()
	m.expanded = 1
	expanded := m.listHeight()

	if expanded >= collapsed {
		t.Errorf("list height %d with a record open, %d without", expanded, collapsed)
	}
}
