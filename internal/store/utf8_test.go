package store

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/GrantPukka/loupe/internal/parse"
)

// badBytes is a logfmt line carrying bytes that are not valid UTF-8, of the
// kind a service emits when it logs a decoded payload it did not decode.
const badBytes = "ts=2026-08-13T14:00:02Z level=error msg=\"decode failed\" raw=\"\x9c\xff\xfe\""

func TestSanitiseEntry(t *testing.T) {
	tests := []struct {
		name    string
		entry   parse.Entry
		want    bool
		wantRaw string
	}{
		{
			name:    "clean line is untouched",
			entry:   parse.Entry{Raw: "all fine here"},
			want:    false,
			wantRaw: "all fine here",
		},
		{
			name:    "invalid raw is replaced",
			entry:   parse.Entry{Raw: "before \xff after"},
			want:    true,
			wantRaw: "before � after",
		},
		{
			name: "invalid message is replaced",
			entry: parse.Entry{
				Raw:    "clean",
				Record: parse.Record{Message: "oops \x9c"},
			},
			want:    true,
			wantRaw: "clean",
		},
		{
			name: "invalid field value is replaced",
			entry: parse.Entry{
				Raw:    "clean",
				Record: parse.Record{Fields: map[string]any{"payload": "\xfe\xfe"}},
			},
			want:    true,
			wantRaw: "clean",
		},
		{
			name: "invalid field key is replaced",
			entry: parse.Entry{
				Raw:    "clean",
				Record: parse.Record{Fields: map[string]any{"na\xffme": "fine"}},
			},
			want:    true,
			wantRaw: "clean",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := tt.entry.Raw
			e := tt.entry

			if got := sanitiseEntry(&e); got != tt.want {
				t.Fatalf("sanitiseEntry = %v, want %v", got, tt.want)
			}
			if e.Raw != tt.wantRaw {
				t.Errorf("raw = %q, want %q", e.Raw, tt.wantRaw)
			}

			if !tt.want {
				if _, ok := e.Fields[RawHexField]; ok {
					t.Error("a clean record must not gain a hex copy")
				}
				return
			}

			// The original bytes have to survive, or this is data loss with a
			// friendlier error message.
			got, ok := e.Fields[RawHexField].(string)
			if !ok {
				t.Fatalf("no %s field on a sanitised record", RawHexField)
			}
			decoded, err := hex.DecodeString(got)
			if err != nil {
				t.Fatalf("decode %s: %v", RawHexField, err)
			}
			if string(decoded) != original {
				t.Errorf("hex round-trip = %q, want %q", decoded, original)
			}
		})
	}
}

// A key the log itself carries must win: the fields bag is the one place an
// unrecognised key is promised to survive.
func TestSanitiseEntryKeepsAnExistingHexField(t *testing.T) {
	e := parse.Entry{
		Raw:    "bad \xff",
		Record: parse.Record{Fields: map[string]any{RawHexField: "mine"}},
	}

	sanitiseEntry(&e)

	if got := e.Fields[RawHexField]; got != "mine" {
		t.Errorf("%s = %v, want the record's own value", RawHexField, got)
	}
}

// The regression this whole file exists for: one line of invalid UTF-8 used to
// abort the ingest of every other line in the file.
func TestIngestSurvivesInvalidUTF8(t *testing.T) {
	dir := logDir(t,
		`{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"a","status":200}`,
		badBytes,
		`{"ts":"2026-08-13T14:00:01Z","level":"error","msg":"b","status":500}`,
	)

	cached := openCached(t, dir, t.TempDir(), CacheOptions{})

	if got := cached.Load.Stats.Records; got != 3 {
		t.Errorf("records = %d, want 3 — no line may be lost to a bad byte", got)
	}
	if got := cached.Load.Stats.InvalidUTF8; got != 1 {
		t.Errorf("InvalidUTF8 = %d, want 1", got)
	}
}

// Case-insensitive search over a corpus containing invalid UTF-8 used to hang
// forever: DuckDB's lower() does not return on such a row. Everything the
// filter DSL compiles for a bare lowercase word runs through here.
func TestLowercaseSearchOverSanitisedData(t *testing.T) {
	dir := logDir(t,
		`{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"ROLLBACK issued"}`,
		badBytes,
	)
	cached := openCached(t, dir, t.TempDir(), CacheOptions{})

	ctx := context.Background()
	res, err := cached.DB.QueryResult(ctx, 0,
		`SELECT line_no FROM logs WHERE lower(raw) LIKE ?`, "%rollback%")
	if err != nil {
		t.Fatalf("lower() query: %v", err)
	}
	if res.RowCount() != 1 {
		t.Errorf("rows = %d, want 1", res.RowCount())
	}
}

// The replacement is visible in the stored text, and the raw bytes are still
// recoverable from the bag.
func TestInvalidUTF8IsRecoverableFromTheStore(t *testing.T) {
	dir := logDir(t, badBytes)
	cached := openCached(t, dir, t.TempDir(), CacheOptions{})

	ctx := context.Background()
	res, err := cached.DB.QueryResult(ctx, 0,
		`SELECT raw, fields->>'$.`+RawHexField+`' FROM logs`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.RowCount() != 1 {
		t.Fatalf("rows = %d, want 1", res.RowCount())
	}

	raw, _ := res.Rows[0][0].(string)
	if !strings.Contains(raw, "�") {
		t.Errorf("stored raw = %q, want replacement characters", raw)
	}

	encoded, _ := res.Rows[0][1].(string)
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode stored hex: %v", err)
	}
	if string(decoded) != badBytes {
		t.Errorf("recovered = %q, want the original bytes", decoded)
	}
}
