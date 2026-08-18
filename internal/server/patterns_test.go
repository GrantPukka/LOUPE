package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPatternsEndpoint(t *testing.T) {
	srv := newTestServer(t)

	var got patternsResponse
	if code := do(t, srv, "GET", "/api/patterns", "", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}

	if got.Templates == 0 || len(got.Patterns) == 0 {
		t.Fatalf("no templates returned: %+v", got)
	}
	if got.Timezone != "UTC" {
		t.Errorf("timezone = %q, want UTC", got.Timezone)
	}

	// Everything the UI needs to render a row and then act on it.
	for _, p := range got.Patterns {
		if p.ID == "" {
			t.Errorf("template %q has no id", p.Template)
		}
		if p.Template == "" {
			t.Errorf("template %s has no text", p.ID)
		}
		if p.Count == 0 {
			t.Errorf("template %q has a zero count", p.Template)
		}
	}

	// The counts have to add up, or a rail built from this understates the
	// data it is summarising.
	var total int64
	for _, p := range got.Patterns {
		total += p.Count
	}
	if total+got.HiddenRecords != got.Records {
		t.Errorf("%d shown + %d hidden != %d records", total, got.HiddenRecords, got.Records)
	}
}

// The endpoint narrows exactly as the CLI does, because it is the same call.
func TestPatternsEndpointRespectsTheFilter(t *testing.T) {
	srv := newTestServer(t)

	var got patternsResponse
	if code := do(t, srv, "GET", "/api/patterns?filter=level:error", "", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}

	if got.Records == 0 {
		t.Fatal("level:error matched no records")
	}

	var all patternsResponse
	do(t, srv, "GET", "/api/patterns", "", &all)
	if got.Records >= all.Records {
		t.Errorf("filtered listing covers %d records, unfiltered %d", got.Records, all.Records)
	}
}

// A template id from the listing must select its records through the ordinary
// query endpoint, or the rail is a dead end.
func TestPatternsEndpointIDsExpandThroughQuery(t *testing.T) {
	srv := newTestServer(t)

	var listing patternsResponse
	do(t, srv, "GET", "/api/patterns", "", &listing)
	if len(listing.Patterns) == 0 {
		t.Fatal("no templates to expand")
	}

	first := listing.Patterns[0]

	var records queryResponse
	code := do(t, srv, "POST", "/api/query",
		`{"filter":"pattern:`+first.ID+`","limit":500}`, &records)
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, records.Columns)
	}

	if records.Total != first.Count {
		t.Errorf("template %q listed %d records but pattern:%s returned %d",
			first.Template, first.Count, first.ID, records.Total)
	}
}

// An unknown id comes back as the CLI's error, suggestions included, rather
// than as an empty listing.
func TestPatternsEndpointErrorsCarryTheFullMessage(t *testing.T) {
	srv := newTestServer(t)

	var got apiError
	code := do(t, srv, "GET", "/api/patterns?filter=pattern:ffffffffffff", "", &got)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(got.Error, "loupe patterns") {
		t.Errorf("error does not say how to list the templates: %q", got.Error)
	}
}

func TestPatternsEndpointRejectsABadLimit(t *testing.T) {
	srv := newTestServer(t)

	var got apiError
	if code := do(t, srv, "GET", "/api/patterns?limit=lots", "", &got); code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

func TestPatternsEndpointNewSince(t *testing.T) {
	srv := newTestServer(t)

	var got patternsResponse
	code := do(t, srv, "GET", "/api/patterns?new_since=1m", "", &got)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	// The cutoff travels with the response, so the UI can state the window
	// rather than inventing one.
	if got.Since.IsZero() {
		t.Error("no cutoff reported for new_since")
	}
	if got.Anchor == "" {
		t.Error("the cutoff does not say what it counted back from")
	}

	var bad apiError
	if code := do(t, srv, "GET", "/api/patterns?new_since=soon", "", &bad); code != http.StatusBadRequest {
		t.Fatalf("status = %d for a bad duration, want 400", code)
	}
}

// An empty listing must be an empty array, not null, or the first filter that
// matches nothing crashes a client that never saw the shape before.
func TestPatternsEndpointEmptyListingIsAnArray(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest("GET", "/api/patterns?filter=level:fatal", nil)
	req.Host = "127.0.0.1:7717"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := string(raw["patterns"]); got == "null" {
		t.Error("an empty listing sent null, want an empty array")
	}
}
