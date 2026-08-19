package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestTraceEndpoint(t *testing.T) {
	srv := newTestServer(t)

	var got traceResponse
	if code := do(t, srv, "GET", "/api/trace?id=a2", "", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}

	if got.ID != "a2" {
		t.Errorf("id = %q, want a2", got.ID)
	}
	if got.Field != "trace_id" {
		t.Errorf("field = %q, want trace_id", got.Field)
	}
	if len(got.Hops) == 0 {
		t.Fatalf("no hops for a trace that exists: %+v", got)
	}
	if got.Timezone != "UTC" {
		t.Errorf("timezone = %q, want UTC", got.Timezone)
	}
}

// The silent-versus-blind distinction is computed server-side, so the browser
// cannot re-derive it differently from the terminal.
func TestTraceEndpointCarriesReach(t *testing.T) {
	srv := newTestServer(t)

	var got traceResponse
	if code := do(t, srv, "GET", "/api/trace?id=a2", "", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}

	if len(got.Reach) == 0 {
		t.Fatal("no reach reported")
	}
	// The fixture's auth.log carries no trace_id at all.
	blind := map[string]bool{}
	for _, r := range got.Blind {
		blind[r.Name] = true
	}
	if !blind["auth"] {
		t.Errorf("blind = %+v, want the source that records no trace_id", got.Blind)
	}
}

// An id that matches nothing is a normal answer, and must be an empty array
// rather than null so the first such response does not crash a client.
func TestTraceEndpointOnAnUnknownID(t *testing.T) {
	srv := newTestServer(t)

	var got traceResponse
	if code := do(t, srv, "GET", "/api/trace?id=nosuchtrace", "", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(got.Hops) != 0 {
		t.Errorf("a made-up id matched %d hops", len(got.Hops))
	}
	// Reach still travels: the UI has to be able to say what could not be
	// checked even when nothing matched.
	if len(got.Reach) == 0 {
		t.Error("no reach reported for an unmatched id")
	}
}

func TestTraceEndpointNeedsAnID(t *testing.T) {
	srv := newTestServer(t)

	var got apiError
	if code := do(t, srv, "GET", "/api/trace", "", &got); code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

// A field the data does not have comes back with the CLI's message.
func TestTraceEndpointRejectsAnUnknownField(t *testing.T) {
	srv := newTestServer(t)

	var got apiError
	code := do(t, srv, "GET", "/api/trace?id=a2&field=nosuchfield", "", &got)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(got.Error, "nosuchfield") {
		t.Errorf("error does not name the field: %q", got.Error)
	}
}

// The UI asks whether a trace view is worth offering at all.
func TestTraceFieldEndpoint(t *testing.T) {
	srv := newTestServer(t)

	var got struct {
		Name  string `json:"name"`
		Field string `json:"field"`
	}
	if code := do(t, srv, "GET", "/api/trace-field", "", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if got.Name != "trace_id" {
		t.Errorf("detected %q, want trace_id", got.Name)
	}
}
