package render

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/VIGIL-OPS/loupe/internal/session"
)

func sample() session.Handoff {
	return session.Handoff{
		Query:       "level:>=error",
		WindowLocal: "02:00:00–07:30:00 BST · Wed 2026-08-13",
		WindowUTC:   "01:00:00–06:30:00 UTC · Wed 2026-08-13",
		Timezone:    "Europe/London",
		Notes:       []string{"last:15m is relative to the newest record"},
		Sources: []session.HandoffSource{
			{File: "checkout-api.log", Format: "jsonl", Bytes: 2104,
				Records: 2104, Timezone: "known (carried in the format)"},
			{File: "payment-worker.log", Format: "log4j", Bytes: 431,
				Records: 431, Timezone: "assumed — no offset in format"},
		},
		Counts: session.HandoffCounts{
			Matched: 412, Shown: 2, ExcludedNoTimestamp: 18, Unparsed: 7, Ingested: 2535,
		},
		Records: []session.HandoffRecord{
			{
				Local: "02:14:06.221", UTC: "01:14:06.221",
				Level: "error", Source: "postgres",
				Text:   "FATAL: remaining connection slots are reserved",
				Raw:    "2026-08-13 01:14:06.221 UTC [20044] ERROR: remaining connection slots",
				Parsed: true,
			},
			{
				Local: "02:14:07.100", UTC: "01:14:07.100",
				Level: "error", Source: "payment-worker",
				Text:        "read timed out\n\tat com.acme.Gateway.charge(Gateway.java:214)",
				Raw:         "2026-08-13 01:14:07.100 [w-1] ERROR c.a.G - read timed out",
				ZoneAssumed: true,
				Parsed:      true,
			},
		},
		Truncated: true,
		Meta: session.HandoffProvenance{
			Tool: "loupe", Version: "v0.6.0", Host: "host-01", User: "alice",
			At: time.Date(2026, 8, 13, 8, 2, 11, 0, time.UTC),
		},
	}
}

func renderMarkdown(t *testing.T, h session.Handoff) string {
	t.Helper()
	var b bytes.Buffer
	if err := WriteHandoff(&b, h, HandoffMarkdown); err != nil {
		t.Fatalf("WriteHandoff: %v", err)
	}
	return b.String()
}

func TestHandoffFormatFromExtension(t *testing.T) {
	tests := map[string]HandoffFormat{
		"incident.md":   HandoffMarkdown,
		"incident.json": HandoffJSON,
		"bundle.zip":    HandoffZip,
		"-":             HandoffMarkdown,
		"noext":         HandoffMarkdown,
	}

	for path, want := range tests {
		t.Run(path, func(t *testing.T) {
			got, err := HandoffFormatFor(path)
			if err != nil {
				t.Fatalf("HandoffFormatFor(%q): %v", path, err)
			}
			if got != want {
				t.Errorf("= %q, want %q", got, want)
			}
		})
	}

	if _, err := HandoffFormatFor("incident.pdf"); err == nil {
		t.Error("an unknown extension was accepted")
	}
}

// The markdown carries every fact docs/HANDOFF.md section 2 requires, in the
// order the receiving engineer reads them.
func TestMarkdownCarriesEveryRequiredFact(t *testing.T) {
	got := renderMarkdown(t, sample())

	required := map[string]string{
		"the window in the local zone": "02:00:00–07:30:00 BST",
		"the window in UTC":            "01:00:00–06:30:00 UTC",
		"the query, verbatim":          "`level:>=error`",
		"the matched count":            "412",
		"the ingested denominator":     "2,535",
		"the excluded count":           "18 records had no parseable timestamp",
		"the unparsed count":           "7 lines matched no parser",
		"the truncation notice":        "showing the first 2 of 412",
		"the source table":             "checkout-api.log",
		"the format of each source":    "log4j",
		"an assumed timezone, flagged": "**assumed — no offset in format**",
		"the records":                  "FATAL: remaining connection slots",
		"the raw lines":                "2026-08-13 01:14:06.221 UTC [20044]",
		"the tool version":             "v0.6.0",
		"who ran it":                   "alice",
		"where it ran":                 "host-01",
		"the resolver's notes":         "relative to the newest record",
	}

	for what, want := range required {
		if !strings.Contains(got, want) {
			t.Errorf("the extract is missing %s (%q)", what, want)
		}
	}
}

// A record whose timezone was assumed is marked where a reader will see it,
// not only in the source table they may not scroll back to.
func TestMarkdownMarksAssumedTimestampsInline(t *testing.T) {
	got := renderMarkdown(t, sample())

	if !strings.Contains(got, "02:14:07.100 ⚠") {
		t.Error("an assumed timestamp is not marked in the record table")
	}
	if !strings.Contains(got, "⚠ marks a timestamp whose timezone was assumed") {
		t.Error("the marker is not explained")
	}
}

// A multi-line record must not break the table, and must say what is missing.
func TestMarkdownHandlesMultiLineMessages(t *testing.T) {
	got := renderMarkdown(t, sample())

	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "| 02:14:07") && strings.Contains(line, "at com.acme") {
			t.Error("a stack trace was inlined into the table row")
		}
	}
	if !strings.Contains(got, "(+1 lines, see raw)") {
		t.Error("the table does not say the message continues")
	}

	// And the whole thing is in the raw section.
	if !strings.Contains(got, "read timed out") {
		t.Error("the raw line is missing")
	}
}

// A pipe in a message must not split a table cell.
func TestMarkdownEscapesTableCells(t *testing.T) {
	h := sample()
	h.Records[0].Text = "GET /a|b HTTP/1.1"

	got := renderMarkdown(t, h)
	if !strings.Contains(got, `/a\|b`) {
		t.Error("a pipe in a message was not escaped")
	}
}

func TestMarkdownWithoutATimeFilter(t *testing.T) {
	h := sample()
	h.WindowLocal, h.WindowUTC = "", ""

	got := renderMarkdown(t, h)
	if !strings.Contains(got, "all time (no time filter)") {
		t.Error("an unbounded extract does not say so")
	}
}

func TestMarkdownRedactionIsDeclared(t *testing.T) {
	h := sample()
	h.Redacted = []string{"user_id", "email"}

	got := renderMarkdown(t, h)
	if !strings.Contains(got, "**Redacted** user_id, email") {
		t.Error("redaction was applied without being declared in the extract")
	}
}

func TestJSONRoundTrips(t *testing.T) {
	var b bytes.Buffer
	if err := WriteHandoff(&b, sample(), HandoffJSON); err != nil {
		t.Fatalf("WriteHandoff: %v", err)
	}

	var got session.Handoff
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatalf("the JSON extract does not parse: %v", err)
	}

	if got.Query != "level:>=error" {
		t.Errorf("Query = %q", got.Query)
	}
	if got.Counts.Matched != 412 {
		t.Errorf("Matched = %d", got.Counts.Matched)
	}
	if len(got.Records) != 2 {
		t.Errorf("got %d records", len(got.Records))
	}
	if !got.Truncated {
		t.Error("the truncation flag was lost")
	}
	// The raw line must survive the round trip; it is the part a receiver
	// trusts over our parsing.
	if got.Records[0].Raw == "" {
		t.Error("the raw line was lost")
	}
}

func TestZipBundle(t *testing.T) {
	var b bytes.Buffer
	if err := WriteHandoff(&b, sample(), HandoffZip); err != nil {
		t.Fatalf("WriteHandoff: %v", err)
	}

	r, err := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if err != nil {
		t.Fatalf("the bundle is not a valid zip: %v", err)
	}

	names := map[string]bool{}
	for _, f := range r.File {
		names[f.Name] = true
	}

	for _, want := range []string{"extract.md", "extract.json", "raw.log"} {
		if !names[want] {
			t.Errorf("the bundle is missing %s", want)
		}
	}

	// raw.log holds the lines verbatim, one per record, for grepping.
	for _, f := range r.File {
		if f.Name != "raw.log" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open raw.log: %v", err)
		}
		var raw bytes.Buffer
		raw.ReadFrom(rc)
		rc.Close()

		if lines := strings.Count(strings.TrimSpace(raw.String()), "\n") + 1; lines != 2 {
			t.Errorf("raw.log has %d lines, want one per record", lines)
		}
	}
}

func TestEmptyExtractStillCarriesContext(t *testing.T) {
	h := sample()
	h.Records = nil
	h.Counts.Matched, h.Counts.Shown = 0, 0
	h.Truncated = false

	got := renderMarkdown(t, h)

	if !strings.Contains(got, "No records matched") {
		t.Error("an empty extract does not say so")
	}
	// The sources and the query still travel, so the reader can see what was
	// searched rather than receiving a bare "nothing".
	if !strings.Contains(got, "checkout-api.log") {
		t.Error("an empty extract dropped the source list")
	}
	if !strings.Contains(got, "`level:>=error`") {
		t.Error("an empty extract dropped the query")
	}
}

func TestHumanBytes(t *testing.T) {
	tests := map[int64]string{
		0: "—", 512: "512B", 2048: "2.0KiB", 1048576: "1.0MiB",
	}
	for n, want := range tests {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
