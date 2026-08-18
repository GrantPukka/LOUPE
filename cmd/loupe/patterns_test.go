package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/GrantPukka/loupe/internal/session"
)

// listing runs the writer and returns what landed on each stream.
//
// The split matters: the templates go to stdout so the listing can be piped,
// and everything about how to read them goes to stderr so a pipe does not
// swallow the caveats.
func listing(set session.PatternSet) (out, status string) {
	var o, s bytes.Buffer
	writePatterns(&o, &s, set, time.UTC, true)
	return o.String(), s.String()
}

func sample(n int) []session.Pattern {
	out := make([]session.Pattern, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, session.Pattern{
			ID:       strings.Repeat("a", 11) + string(rune('0'+i)),
			Template: "template " + string(rune('a'+i)),
			Count:    int64(100 - i),
			Sources:  []string{"app"},
			Example:  "example",
		})
	}
	return out
}

func TestWritePatternsListsTemplatesOnStdout(t *testing.T) {
	set := session.PatternSet{
		Patterns:  sample(3),
		Templates: 3,
		Records:   297,
	}

	out, status := listing(set)

	for _, want := range []string{"template a", "template b", "template c"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout is missing %q:\n%s", want, out)
		}
	}
	// Counts are grouped, because a count is the number a reader compares at
	// a glance and 186468 does not compare.
	if !strings.Contains(status, "297 records") {
		t.Errorf("footer does not state the record total:\n%s", status)
	}
	if strings.Contains(out, "templates covering") {
		t.Error("the footer was written to stdout, where a pipe would take it as data")
	}
}

// Truncation is never silent, and what is missing is counted.
func TestWritePatternsDeclaresTruncation(t *testing.T) {
	set := session.PatternSet{
		Patterns:      sample(2),
		Templates:     931,
		Records:       186468,
		Truncated:     true,
		Hidden:        929,
		HiddenRecords: 80484,
	}

	_, status := listing(set)

	for _, want := range []string{"929 templates", "80,484 records", "--all"} {
		if !strings.Contains(status, want) {
			t.Errorf("truncation notice is missing %q:\n%s", want, status)
		}
	}
}

// The share of the listing that came from unreadable lines is separated out,
// or the headline count reads as the collapse rule having failed.
func TestWritePatternsSeparatesUnparsedTemplates(t *testing.T) {
	set := session.PatternSet{
		Patterns:          sample(1),
		Templates:         931,
		Records:           186468,
		UnparsedTemplates: 846,
		UnparsedRecords:   1499,
	}

	_, status := listing(set)

	if !strings.Contains(status, "846 of those templates") {
		t.Errorf("the unparsed share is not reported:\n%s", status)
	}
	if !strings.Contains(status, "parsed:true") {
		t.Errorf("the notice does not offer the term that excludes them:\n%s", status)
	}
}

// A --new-since cutoff is stated in local and UTC before any result, so nobody
// has to do offset arithmetic to know what "new" meant.
func TestWritePatternsStatesTheCutoffInBothZones(t *testing.T) {
	sydney, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}

	set := session.PatternSet{
		Patterns:    sample(1),
		Templates:   3,
		Records:     10,
		Since:       time.Date(2026, 8, 13, 14, 5, 0, 0, time.UTC),
		Anchor:      "the newest record",
		Established: 2,
	}

	var out, status bytes.Buffer
	writePatterns(&out, &status, set, sydney, true)
	got := status.String()

	if !strings.Contains(got, "UTC") {
		t.Errorf("the cutoff is not stated in UTC:\n%s", got)
	}
	if !strings.Contains(got, "AEST") && !strings.Contains(got, "AEDT") {
		t.Errorf("the cutoff is not stated in the display zone:\n%s", got)
	}
	if !strings.Contains(got, "the newest record") {
		t.Errorf("the cutoff does not say what it counted back from:\n%s", got)
	}
	if !strings.Contains(got, "2 templates already present") {
		t.Errorf("the established templates are not accounted for:\n%s", got)
	}
}

// Nothing new is a real answer, not an empty screen.
func TestWritePatternsExplainsNoNewTemplates(t *testing.T) {
	set := session.PatternSet{
		Templates:   12,
		Records:     400,
		Since:       time.Date(2026, 8, 13, 14, 5, 0, 0, time.UTC),
		Anchor:      "the newest record",
		Established: 12,
	}

	_, status := listing(set)

	if !strings.Contains(status, "No new templates") {
		t.Errorf("an empty new-since listing does not explain itself:\n%s", status)
	}
	if !strings.Contains(status, "12 templates") {
		t.Errorf("it does not say how many were already present:\n%s", status)
	}
}

func TestWritePatternsOnNoMatches(t *testing.T) {
	_, status := listing(session.PatternSet{})

	if !strings.Contains(status, "No records matched") {
		t.Errorf("an empty listing does not explain itself:\n%s", status)
	}
}

// "1 record have no timestamp" is the kind of small wrongness that makes a
// tool feel unfinished.
func TestWritePatternsAgreesItsVerbs(t *testing.T) {
	one := session.PatternSet{Patterns: sample(1), Templates: 1, Records: 1, Undated: 1}
	many := session.PatternSet{Patterns: sample(1), Templates: 1, Records: 9, Undated: 2}

	if _, status := listing(one); !strings.Contains(status, "1 record has no timestamp") {
		t.Errorf("singular is wrong:\n%s", status)
	}
	if _, status := listing(many); !strings.Contains(status, "2 records have no timestamp") {
		t.Errorf("plural is wrong:\n%s", status)
	}
}
