package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/GrantPukka/loupe/internal/session"
)

// breakdown runs the writer and returns what landed on each stream.
//
// The split matters: values go to stdout so the list can be piped, and the
// caveats go to stderr so a pipe cannot swallow them.
func breakdown(set session.TopSet) (out, status string) {
	var o, s bytes.Buffer
	writeTop(&o, &s, set, true)
	return o.String(), s.String()
}

func sampleTop() session.TopSet {
	return session.TopSet{
		Field:    "path",
		Matched:  500,
		Present:  412,
		Absent:   88,
		Distinct: 3,
		Values: []session.TopValue{
			{Value: "/api/checkout", Count: 300, Share: 300.0 / 412.0},
			{Value: "/api/cart", Count: 100, Share: 100.0 / 412.0},
			{Value: "/healthz", Count: 12, Share: 12.0 / 412.0},
		},
	}
}

func TestWriteTopListsValuesOnStdout(t *testing.T) {
	out, status := breakdown(sampleTop())

	for _, want := range []string{"/api/checkout", "/api/cart", "/healthz"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout is missing %q:\n%s", want, out)
		}
	}
	// Counts are grouped, because a count is the number a reader compares.
	if !strings.Contains(out, "300") {
		t.Errorf("stdout is missing the counts:\n%s", out)
	}
	if strings.Contains(out, "values of path") {
		t.Error("the footer was written to stdout, where a pipe would take it as data")
	}
	if !strings.Contains(status, "3 values of path across 412 records") {
		t.Errorf("footer does not state the totals:\n%s", status)
	}
}

// A percentage is the point: 412 of 33,000 reads differently from 412 of 500.
func TestWriteTopShowsShares(t *testing.T) {
	out, _ := breakdown(sampleTop())

	// 300 of 412 is 72.8%.
	if !strings.Contains(out, "72.8%") {
		t.Errorf("stdout does not show the share:\n%s", out)
	}
}

// Records missing the field sit outside the percentages, so the reader has to
// be told they exist.
func TestWriteTopDeclaresRecordsMissingTheField(t *testing.T) {
	_, status := breakdown(sampleTop())

	if !strings.Contains(status, "88 records matched the filter but carry no path") {
		t.Errorf("the absent records are not reported:\n%s", status)
	}
	// And it offers the term that finds them.
	if !strings.Contains(status, "path:none") {
		t.Errorf("the notice does not offer the term that finds them:\n%s", status)
	}
}

func TestWriteTopDeclaresTruncation(t *testing.T) {
	set := sampleTop()
	set.Truncated = true
	set.Hidden = 17
	set.HiddenRecords = 240

	_, status := breakdown(set)

	for _, want := range []string{"17 values", "240 records", "--all"} {
		if !strings.Contains(status, want) {
			t.Errorf("truncation notice is missing %q:\n%s", want, status)
		}
	}
}

// A field present but empty is a real answer. A blank line would read as a
// rendering fault instead.
func TestWriteTopNamesAnEmptyValue(t *testing.T) {
	set := sampleTop()
	set.Values[2].Value = ""

	out, _ := breakdown(set)
	if !strings.Contains(out, "(empty)") {
		t.Errorf("an empty value rendered as nothing:\n%s", out)
	}
}

// A log line can contain control characters. A terminal renders them as damage
// or as nothing, which reads as a fault in the tool rather than in the log.
func TestWriteTopMasksControlCharacters(t *testing.T) {
	set := sampleTop()
	set.Values[0].Value = "/api\x00\x00/checkout"

	out, _ := breakdown(set)
	if strings.ContainsRune(out, 0) {
		t.Error("a NUL byte reached the terminal")
	}
	if !strings.Contains(out, "<ctl>") {
		t.Errorf("the control characters are not named:\n%s", out)
	}
}

// Nothing matched, and nothing carrying the field, are different answers and
// both deserve an explanation rather than a blank screen.
func TestWriteTopExplainsEmptyResults(t *testing.T) {
	_, nothing := breakdown(session.TopSet{Field: "path"})
	if !strings.Contains(nothing, "No records matched") {
		t.Errorf("an empty breakdown does not explain itself:\n%s", nothing)
	}

	_, absent := breakdown(session.TopSet{Field: "path", Matched: 40, Absent: 40})
	if !strings.Contains(absent, "carry a value for path") {
		t.Errorf("a breakdown with no values does not explain itself:\n%s", absent)
	}
	if !strings.Contains(absent, "40") {
		t.Errorf("it does not say how many records were checked:\n%s", absent)
	}
}

// The bar is scaled to the largest value, so the shape of the distribution is
// visible without reading numbers — and a single record still draws something.
func TestShareBarScalesToTheLargest(t *testing.T) {
	const width = 10

	if got := shareBar(10, 10, width); strings.Count(got, "█") != width {
		t.Errorf("the largest value drew %d blocks, want %d", strings.Count(got, "█"), width)
	}
	if got := shareBar(5, 10, width); strings.Count(got, "█") != width/2 {
		t.Errorf("half the largest drew %d blocks, want %d", strings.Count(got, "█"), width/2)
	}
	if got := shareBar(1, 100_000, width); strings.Count(got, "█") != 1 {
		t.Error("a tiny non-zero count drew nothing; it must be distinguishable from none")
	}
	if got := shareBar(0, 10, width); strings.Contains(got, "█") {
		t.Error("a zero count drew a block")
	}
	// Every bar is the same width, or the column after it does not line up.
	for _, n := range []int64{0, 1, 5, 10} {
		if got := len([]rune(shareBar(n, 10, width))); got != width {
			t.Errorf("bar for %d is %d runes wide, want %d", n, got, width)
		}
	}
}
