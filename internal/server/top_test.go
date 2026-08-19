package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTopEndpoint(t *testing.T) {
	srv := newTestServer(t)

	var got topResponse
	if code := do(t, srv, "GET", "/api/top?field=level", "", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}

	if got.Field != "level" {
		t.Errorf("field = %q, want level", got.Field)
	}
	if len(got.Values) == 0 {
		t.Fatalf("no values returned: %+v", got)
	}
	if got.Timezone != "UTC" {
		t.Errorf("timezone = %q, want UTC", got.Timezone)
	}

	// Descending, and the shares are the fraction of records carrying the
	// field — the browser must not have to work either out for itself.
	previous := int64(1 << 62)
	var shares float64
	for _, v := range got.Values {
		if v.Count > previous {
			t.Errorf("value %q sorted after a smaller count", v.Value)
		}
		previous = v.Count
		shares += v.Share
	}
	if shares < 0.999 || shares > 1.001 {
		t.Errorf("shares sum to %v, want 1", shares)
	}
}

// The counts have to reconcile, or a breakdown built from this understates the
// data it is summarising.
func TestTopEndpointCountsReconcile(t *testing.T) {
	srv := newTestServer(t)

	var got topResponse
	if code := do(t, srv, "GET", "/api/top?field=level&limit=-1", "", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}

	var counted int64
	for _, v := range got.Values {
		counted += v.Count
	}
	if counted != got.Present {
		t.Errorf("values sum to %d but present = %d", counted, got.Present)
	}
	if got.Present+got.Absent != got.Matched {
		t.Errorf("%d present + %d absent != %d matched", got.Present, got.Absent, got.Matched)
	}
}

// Records missing the field sit outside the percentages, so the count travels
// with the response for the UI to state.
func TestTopEndpointCarriesTheAbsentCount(t *testing.T) {
	srv := newTestServer(t)

	// The fixture's logfmt source carries no status field.
	var got topResponse
	if code := do(t, srv, "GET", "/api/top?field=status", "", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if got.Absent == 0 {
		t.Errorf("absent = 0, but some fixture records carry no status: %+v", got)
	}
}

func TestTopEndpointRespectsTheFilter(t *testing.T) {
	srv := newTestServer(t)

	var all, filtered topResponse
	do(t, srv, "GET", "/api/top?field=level&limit=-1", "", &all)

	code := do(t, srv, "GET", "/api/top?field=level&filter=level:error&limit=-1", "", &filtered)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(filtered.Values) != 1 || filtered.Values[0].Value != "error" {
		t.Errorf("filtered breakdown = %+v, want just error", filtered.Values)
	}
	if filtered.Present >= all.Present {
		t.Errorf("filtered covers %d records, unfiltered %d", filtered.Present, all.Present)
	}
}

// An unknown field comes back with the CLI's spelling suggestion, not an empty
// breakdown.
func TestTopEndpointRejectsAnUnknownField(t *testing.T) {
	srv := newTestServer(t)

	var got apiError
	code := do(t, srv, "GET", "/api/top?field=levl", "", &got)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(got.Error, "level") {
		t.Errorf("error does not suggest the field meant: %q", got.Error)
	}
}

func TestTopEndpointNeedsAField(t *testing.T) {
	srv := newTestServer(t)

	var got apiError
	if code := do(t, srv, "GET", "/api/top", "", &got); code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

func TestTopEndpointRejectsABadLimit(t *testing.T) {
	srv := newTestServer(t)

	var got apiError
	if code := do(t, srv, "GET", "/api/top?field=level&limit=lots", "", &got); code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

// An empty breakdown must be an array, not null, or the first filter that
// matches nothing crashes a client that never saw the shape before.
func TestTopEndpointEmptyBreakdownIsAnArray(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest("GET", "/api/top?field=level&filter=level:fatal", nil)
	req.Host = "127.0.0.1:7717"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := string(raw["values"]); got == "null" {
		t.Error("an empty breakdown sent null, want an empty array")
	}
}
