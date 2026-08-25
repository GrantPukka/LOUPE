package render

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/GrantPukka/loupe/internal/store"
)

func brisbane(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Australia/Brisbane")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	return loc
}

// A TIMESTAMP a user computed is a naive value and must be shown exactly as
// computed. Converting it moved literal timestamps ten hours and a day, in the
// one command whose whole purpose is answering what the DSL cannot.
func TestUserSQLDoesNotShiftNaiveTimestamps(t *testing.T) {
	naive := time.Date(2026, 8, 20, 22, 32, 2, 0, time.UTC)

	res := store.Result{
		Columns: []string{"as_timestamp", "ts"},
		Types:   []string{"TIMESTAMP", "TIMESTAMP"},
		Rows:    [][]any{{naive, naive}},
	}

	tests := []struct {
		name    string
		userSQL bool
		want    []string
	}{
		{
			name:    "user sql leaves its own column alone but converts ts",
			userSQL: true,
			want:    []string{"2026-08-20 22:32:02.000", "2026-08-21 08:32:02.000"},
		},
		{
			// Loupe's own compiler only derives timestamps from ts, so every
			// column in its output is a real instant.
			name:    "loupe's own sql converts both",
			userSQL: false,
			want:    []string{"2026-08-21 08:32:02.000", "2026-08-21 08:32:02.000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := New(&buf, Options{
				Format:   FormatTable,
				Location: brisbane(t),
				UserSQL:  tt.userSQL,
				Width:    200,
			})
			if err := w.Result(res); err != nil {
				t.Fatalf("Result: %v", err)
			}

			for _, want := range tt.want {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("output missing %q:\n%s", want, buf.String())
				}
			}
		})
	}
}

// A column DuckDB typed as carrying a zone is unambiguous whoever wrote it.
func TestUserSQLConvertsZonedTimestamps(t *testing.T) {
	res := store.Result{
		Columns: []string{"at"},
		Types:   []string{"TIMESTAMP WITH TIME ZONE"},
		Rows:    [][]any{{time.Date(2026, 8, 20, 22, 32, 2, 0, time.UTC)}},
	}

	var buf bytes.Buffer
	w := New(&buf, Options{Format: FormatTable, Location: brisbane(t), UserSQL: true, Width: 200})
	if err := w.Result(res); err != nil {
		t.Fatalf("Result: %v", err)
	}
	if !strings.Contains(buf.String(), "2026-08-21 08:32:02.000") {
		t.Errorf("a zoned timestamp should be converted:\n%s", buf.String())
	}
}

// The machine formats have to agree with the table, or a handoff disagrees with
// the screen.
func TestNaiveTimestampsInNDJSON(t *testing.T) {
	res := store.Result{
		Columns: []string{"as_timestamp"},
		Types:   []string{"TIMESTAMP"},
		Rows:    [][]any{{time.Date(2026, 8, 20, 22, 32, 2, 0, time.UTC)}},
	}

	var buf bytes.Buffer
	w := New(&buf, Options{Format: FormatNDJSON, Location: brisbane(t), UserSQL: true})
	if err := w.Result(res); err != nil {
		t.Fatalf("Result: %v", err)
	}
	if !strings.Contains(buf.String(), `"2026-08-20T22:32:02"`) {
		t.Errorf("ndjson shifted a naive timestamp:\n%s", buf.String())
	}
}

// What the announcement names has to be exactly what was left verbatim.
func TestVerbatimTimestamps(t *testing.T) {
	res := store.Result{
		Columns: []string{"ts", "as_timestamp", "at", "n", "label"},
		Types:   []string{"TIMESTAMP", "TIMESTAMP", "TIMESTAMP WITH TIME ZONE", "BIGINT", "VARCHAR"},
	}

	got := VerbatimTimestamps(Options{UserSQL: true}, res)
	if len(got) != 1 || got[0] != "as_timestamp" {
		t.Errorf("VerbatimTimestamps = %v, want [as_timestamp]", got)
	}

	if got := VerbatimTimestamps(Options{}, res); got != nil {
		t.Errorf("loupe's own output converts everything, so nothing to announce: %v", got)
	}
}
