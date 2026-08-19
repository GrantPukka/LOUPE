package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// EC006 gave the filter language aggregation on the command line, and CLAUDE.md
// requires a capability exist in the CLI before it appears in the UI. Until the
// browser can render a summary, a `stats` clause typed into the filter box has
// to be refused with the reason — never ignored, which would show a listing that
// answers a different question from the one that was asked.
func TestFilterBoxRefusesAnAggregation(t *testing.T) {
	srv := newTestServer(t)
	const filter = "stats count() by level"

	check := func(what string, code int, got apiError) {
		t.Helper()
		if code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want %d", what, code, http.StatusBadRequest)
		}
		if !strings.Contains(got.Error, filter) {
			t.Errorf("%s error does not name the clause: %q", what, got.Error)
		}
	}

	var query apiError
	check("POST /api/query",
		do(t, srv, "POST", "/api/query", `{"filter":"`+filter+`"}`, &query), query)

	var histogram apiError
	check("POST /api/histogram",
		do(t, srv, "POST", "/api/histogram", `{"filter":"`+filter+`"}`, &histogram), histogram)

	var top apiError
	check("GET /api/top",
		do(t, srv, "GET", "/api/top?field=level&filter="+url.QueryEscape(filter), "", &top), top)
}
