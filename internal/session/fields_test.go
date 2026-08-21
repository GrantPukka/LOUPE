package session

import (
	"context"
	"strings"
	"testing"
)

// fieldsFixture has a field on every record, one on some, one that is numeric
// on most records and text on the rest, one that holds two JSON types, and one
// no record carries under a level:error filter.
func fieldsFixture(t *testing.T) *Session {
	t.Helper()

	return openFixture(t,
		`{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"a","path":"/api/cart","latency_ms":100,"region":"eu-west-1"}`,
		`{"ts":"2026-08-13T14:00:01Z","level":"info","msg":"b","path":"/api/cart","latency_ms":200,"region":"eu-west-1"}`,
		`{"ts":"2026-08-13T14:00:02Z","level":"info","msg":"c","path":"/healthz","latency_ms":50,"region":"us-east-1"}`,
		`{"ts":"2026-08-13T14:00:03Z","level":"info","msg":"d","path":"/healthz","latency_ms":"slow","region":"us-east-1"}`,
		`{"ts":"2026-08-13T14:00:04Z","level":"error","msg":"e","path":"/api/checkout","status":500}`,
		`{"ts":"2026-08-13T14:00:05Z","level":"error","msg":"f","path":"/api/checkout","status":"5xx"}`,
	)
}

func fieldsOf(t *testing.T, sess *Session, filter string) FieldSet {
	t.Helper()

	set, err := sess.Fields(context.Background(), plan(t, sess, filter), FieldQuery{Limit: -1})
	if err != nil {
		t.Fatalf("Fields(%q): %v", filter, err)
	}
	return set
}

func field(t *testing.T, set FieldSet, name string) FieldInfo {
	t.Helper()

	for _, f := range set.Fields {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no field %q in %d listed: %+v", name, len(set.Fields), fieldNames(set))
	return FieldInfo{}
}

func fieldNames(set FieldSet) []string {
	out := make([]string, len(set.Fields))
	for i, f := range set.Fields {
		out[i] = f.Name
	}
	return out
}

func has(set FieldSet, name string) bool {
	for _, f := range set.Fields {
		if f.Name == name {
			return true
		}
	}
	return false
}

// The headline: every field a filter could name, with the numbers that decide
// whether naming it is a good idea.
func TestFieldsListsWhatCanBeFiltered(t *testing.T) {
	set := fieldsOf(t, fieldsFixture(t), "")

	if set.Matched != 6 {
		t.Errorf("matched = %d, want 6", set.Matched)
	}

	// A built-in column every record has.
	level := field(t, set, "level")
	if level.Records != 6 || level.Distinct != 2 {
		t.Errorf("level = %d records, %d distinct; want 6 and 2", level.Records, level.Distinct)
	}
	if level.Coverage != 1 {
		t.Errorf("level coverage = %v, want 1", level.Coverage)
	}
	if !level.Column {
		t.Error("level was not reported as a real column")
	}

	// A field only some records carry.
	status := field(t, set, "status")
	if status.Records != 2 {
		t.Errorf("status = %d records, want 2", status.Records)
	}
	if want := 2.0 / 6.0; status.Coverage != want {
		t.Errorf("status coverage = %v, want %v", status.Coverage, want)
	}

	// And the fields the DSL accepts as aliases are not listed twice.
	if has(set, "msg") || has(set, "line") {
		t.Errorf("an alias was listed as its own field: %v", fieldNames(set))
	}
}

// Examples are what make the listing worth reading: they say what a value looks
// like without running a second command.
func TestFieldsShowExampleValues(t *testing.T) {
	set := fieldsOf(t, fieldsFixture(t), "")

	region := field(t, set, "region")
	if len(region.Examples) == 0 {
		t.Fatal("region has no examples")
	}
	for _, v := range region.Examples {
		if v != "eu-west-1" && v != "us-east-1" {
			t.Errorf("example %q is not a value region holds", v)
		}
	}

	// No more than the listing promises, however many values there are.
	for _, f := range set.Fields {
		if len(f.Examples) > fieldExamples {
			t.Errorf("%s showed %d examples, want at most %d", f.Name, len(f.Examples), fieldExamples)
		}
	}
}

// The type is reported in the tool's own words, not the database's: a user
// filtering on a field does not care that an integer arrived as a UBIGINT.
func TestFieldsReportTypes(t *testing.T) {
	set := fieldsOf(t, fieldsFixture(t), "")

	tests := map[string]string{
		"ts":       "timestamp",
		"level":    "string",
		"line_no":  "integer",
		"parsed":   "boolean",
		"path":     "string",
		"ts_zoned": "boolean",
	}

	for name, want := range tests {
		if got := field(t, set, name).Type; got != want {
			t.Errorf("%s type = %q, want %q", name, got, want)
		}
	}
}

// The warning this command exists to give: a field that is a number on most
// records and text on the rest will silently lose the rest to latency_ms:>N.
func TestFieldsWarnAboutPartlyNumericFields(t *testing.T) {
	set := fieldsOf(t, fieldsFixture(t), "")

	latency := field(t, set, "latency_ms")
	if latency.Records != 4 || latency.Numeric != 3 {
		t.Fatalf("latency_ms = %d records, %d numeric; want 4 and 3",
			latency.Records, latency.Numeric)
	}
	if !latency.PartlyNumeric() {
		t.Error("latency_ms was not flagged as partly numeric")
	}

	// A field that is entirely numeric is not flagged, and neither is one that
	// is entirely text: the warning has to stand out from the fields it does
	// not apply to.
	for _, name := range []string{"line_no", "seq", "level", "path", "message"} {
		if field(t, set, name).PartlyNumeric() {
			t.Errorf("%s was flagged as partly numeric", name)
		}
	}
}

// Three log messages that happen to be bare numbers do not make `message` a
// numeric field. A note on every text column would bury the real one.
func TestPartlyNumericNeedsAMajority(t *testing.T) {
	tests := []struct {
		name             string
		records, numeric int64
		want             bool
	}{
		{"all numbers", 100, 100, false},
		{"no numbers", 100, 0, false},
		{"almost all numbers", 100, 99, true},
		{"a bare majority", 100, 51, true},
		{"exactly half", 100, 50, false},
		{"a handful", 100, 3, false},
		{"no records", 0, 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := FieldInfo{Records: tc.records, Numeric: tc.numeric}
			if got := f.PartlyNumeric(); got != tc.want {
				t.Errorf("%d of %d numeric: PartlyNumeric = %v, want %v",
					tc.numeric, tc.records, got, tc.want)
			}
		})
	}
}

// A filter narrows the question, which is most of the value: what the failing
// records carry is a shorter and more interesting list than what everything
// carries.
func TestFieldsRespectsTheFilter(t *testing.T) {
	set := fieldsOf(t, fieldsFixture(t), "level:error")

	if set.Matched != 2 {
		t.Errorf("matched = %d, want 2", set.Matched)
	}
	if !has(set, "status") {
		t.Errorf("status is on both error records but was not listed: %v", fieldNames(set))
	}

	// A field no matching record carries is counted rather than listed as a
	// row of zeroes — and the count is what separates "missing from my results"
	// from "missing from the data".
	if has(set, "region") {
		t.Error("region is on no error record but was listed")
	}
	if set.Absent == 0 {
		t.Error("fields absent from the matching records were not counted")
	}
	if set.Known <= int64(len(set.Fields)) {
		t.Errorf("known = %d, but %d were listed — the data holds more",
			set.Known, len(set.Fields))
	}
}

// Best covered first, because the fields on every record are the ones a filter
// is most likely to want.
func TestFieldsSortByCoverage(t *testing.T) {
	set := fieldsOf(t, fieldsFixture(t), "")

	previous := int64(1 << 62)
	for i, f := range set.Fields {
		if f.Records > previous {
			t.Errorf("field %d (%s, %d) sorted after a better covered one (%d)",
				i, f.Name, f.Records, previous)
		}
		previous = f.Records
	}
}

// A limit cuts the list, and what it cut is counted.
func TestFieldsTruncationStatesWhatItCut(t *testing.T) {
	sess := fieldsFixture(t)

	all := fieldsOf(t, sess, "")
	set, err := sess.Fields(context.Background(), plan(t, sess, ""), FieldQuery{Limit: 3})
	if err != nil {
		t.Fatalf("Fields: %v", err)
	}

	if len(set.Fields) != 3 {
		t.Fatalf("got %d field(s), want 3", len(set.Fields))
	}
	if !set.Truncated {
		t.Error("a cut listing did not declare itself truncated")
	}
	if want := int64(len(all.Fields) - 3); set.Hidden != want {
		t.Errorf("hidden = %d, want %d", set.Hidden, want)
	}
}

// Nothing matched is a different answer from nothing to describe, and the
// caller needs to be able to tell them apart.
func TestFieldsWithNoMatchingRecords(t *testing.T) {
	set := fieldsOf(t, fieldsFixture(t), "message~nothingmatchesthis")

	if set.Matched != 0 {
		t.Errorf("matched = %d, want 0", set.Matched)
	}
	if len(set.Fields) != 0 {
		t.Errorf("got %d field(s) over no records: %v", len(set.Fields), fieldNames(set))
	}
	if set.Known == 0 {
		t.Error("the data's own field count was lost")
	}
}

// The listing is deterministic, so one run can be compared against another.
func TestFieldsAreDeterministic(t *testing.T) {
	sess := fieldsFixture(t)

	first, second := fieldsOf(t, sess, ""), fieldsOf(t, sess, "")
	if strings.Join(fieldNames(first), ",") != strings.Join(fieldNames(second), ",") {
		t.Errorf("two runs listed different fields:\n  %v\n  %v", fieldNames(first), fieldNames(second))
	}
}

// A bag field holding more than one JSON type is worth naming: a comparison
// casts, and the values of the other type go quietly missing.
func TestFieldsReportMixedTypes(t *testing.T) {
	set := fieldsOf(t, fieldsFixture(t), "")

	status := field(t, set, "status")
	if !status.Mixed() {
		t.Fatalf("status holds a number and a string but was reported as %q only", status.Type)
	}
	if strings.Join(status.Types, ",") != "integer,string" {
		t.Errorf("status types = %v, want [integer string]", status.Types)
	}
	if status.Column {
		t.Error("status was reported as a real column; it is too rare to have been promoted")
	}

	// A consistently typed field carries no list, so the table has nothing to
	// mark and the footer has nothing to say.
	for _, name := range []string{"level", "path", "line_no"} {
		if f := field(t, set, name); f.Mixed() {
			t.Errorf("%s was reported as mixed: %v", name, f.Types)
		}
	}
}

// A promoted field is read from a real column, which is why a filter on it is
// fast — and the listing says which is which, because that is the answer to
// "why is this one slower".
func TestFieldsReportWhereAValueIsStored(t *testing.T) {
	set := fieldsOf(t, fieldsFixture(t), "")

	for _, name := range []string{"level", "ts", "message", "path"} {
		if !field(t, set, name).Column {
			t.Errorf("%s should be read from a real column", name)
		}
	}
	if field(t, set, "status").Column {
		t.Error("status is below the promotion threshold and should be read from the bag")
	}
}
