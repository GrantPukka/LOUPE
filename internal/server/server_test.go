package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VIGIL-OPS/loupe/internal/session"
)

// fixture writes a small mixed-format directory and opens a session over it.
func fixture(t *testing.T) *session.Session {
	t.Helper()
	dir := t.TempDir()

	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	write("api.log", strings.Join([]string{
		`{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"started","status":200,"trace_id":"a1"}`,
		`{"ts":"2026-08-13T14:01:00Z","level":"error","msg":"upstream timeout","status":502,"trace_id":"a2"}`,
		`{"ts":"2026-08-13T14:02:00Z","level":"warn","msg":"rate limited","status":429,"trace_id":"a3"}`,
		`{"level":"error","msg":"no timestamp on this one","status":500,"trace_id":"a4"}`,
		`not json at all`,
	}, "\n")+"\n")

	write("auth.log", strings.Join([]string{
		`ts=2026-08-13T14:00:30Z level=info msg="token validated" user_id=u_1`,
		`ts=2026-08-13T14:01:30Z level=debug msg="refreshing cache" user_id=u_2`,
	}, "\n")+"\n")

	sess, err := session.Open(context.Background(), session.Options{
		Path:     dir,
		Location: time.UTC,
		NoCache:  true,
	})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	t.Cleanup(func() { sess.Close() })

	return sess
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return New(fixture(t), Options{})
}

// do sends a request and decodes the JSON response.
func do(t *testing.T, srv *Server, method, path, body string, into any) int {
	t.Helper()

	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}

	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if into != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
			t.Fatalf("decode %s %s: %v\nbody: %s", method, path, err, rec.Body.String())
		}
	}
	return rec.Code
}

func TestSchemaEndpoint(t *testing.T) {
	srv := newTestServer(t)

	var got schemaResponse
	if code := do(t, srv, "GET", "/api/schema", "", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}

	if got.Timezone != "UTC" {
		t.Errorf("timezone = %q, want UTC", got.Timezone)
	}
	if got.Records != 7 {
		t.Errorf("records = %d, want 7", got.Records)
	}

	// The counts a user needs to trust the data must be here, not only in the
	// terminal.
	if got.Unparsed != 1 {
		t.Errorf("unparsed = %d, want 1", got.Unparsed)
	}
	if got.NoTimestamp != 2 {
		t.Errorf("no_timestamp = %d, want 2", got.NoTimestamp)
	}

	names := map[string]columnInfo{}
	for _, c := range got.Columns {
		names[c.Name] = c
	}
	for _, want := range []string{"ts", "level", "message", "source", "raw"} {
		if _, ok := names[want]; !ok {
			t.Errorf("built-in column %q missing", want)
		}
	}
	if c, ok := names["status"]; !ok {
		t.Error("promoted column status missing")
	} else if !c.Promoted || c.Type != "BIGINT" {
		t.Errorf("status = %+v, want a promoted BIGINT", c)
	}

	if len(got.Sources) != 2 {
		t.Fatalf("got %d sources, want 2", len(got.Sources))
	}
	// The known-or-assumed verdict must reach the UI, for the same reason
	// `loupe sources` prints it: an assumption nobody can see is one nobody
	// can check.
	for _, s := range got.Sources {
		if s.Timezone == "" {
			t.Errorf("source %s has no timezone verdict", s.File)
		}
	}
}

func TestQueryEndpoint(t *testing.T) {
	srv := newTestServer(t)

	var got queryResponse
	code := do(t, srv, "POST", "/api/query", `{"filter":"level:>=warn"}`, &got)
	if code != http.StatusOK {
		t.Fatalf("status = %d: %+v", code, got)
	}

	if got.Total != 3 {
		t.Errorf("total = %d, want 3", got.Total)
	}
	if len(got.Rows) != 3 {
		t.Errorf("got %d rows, want 3", len(got.Rows))
	}
	// A client rendering UTC instants without knowing the display zone shows
	// somebody else's clock.
	if got.Timezone != "UTC" {
		t.Errorf("timezone = %q", got.Timezone)
	}
}

func TestQueryPaging(t *testing.T) {
	srv := newTestServer(t)

	var first, second queryResponse
	do(t, srv, "POST", "/api/query", `{"filter":"","limit":2}`, &first)
	do(t, srv, "POST", "/api/query", `{"filter":"","limit":2,"offset":2}`, &second)

	if len(first.Rows) != 2 || len(second.Rows) != 2 {
		t.Fatalf("got %d and %d rows, want 2 each", len(first.Rows), len(second.Rows))
	}
	if sameRow(first.Rows[0], second.Rows[0]) {
		t.Error("offset returned the same first row")
	}

	// A truncated page must say so, or a UI cannot tell the user they are
	// looking at a slice.
	if !first.Truncated {
		t.Error("a limited page did not report truncation")
	}
	if first.Total != 7 {
		t.Errorf("total = %d, want the full 7", first.Total)
	}
}

func TestQuerySortOrder(t *testing.T) {
	srv := newTestServer(t)

	var asc, desc queryResponse
	do(t, srv, "POST", "/api/query", `{"filter":"","limit":1,"sort":"time"}`, &asc)
	do(t, srv, "POST", "/api/query", `{"filter":"","limit":1,"sort":"-time"}`, &desc)

	if sameRow(asc.Rows[0], desc.Rows[0]) {
		t.Error("ascending and descending returned the same first row")
	}
}

// A time filter's window and every assumption behind it must reach the UI, not
// just the terminal.
func TestQueryReportsTheWindowAndItsCaveats(t *testing.T) {
	srv := newTestServer(t)

	var got queryResponse
	do(t, srv, "POST", "/api/query", `{"filter":"last:2m"}`, &got)

	if got.Window == nil {
		t.Fatal("no window reported for a time filter")
	}
	if !strings.Contains(got.Window.Description, "UTC") {
		t.Errorf("window description does not state the zone: %q", got.Window.Description)
	}
	// last: anchors to the newest record, and saying so is required.
	if len(got.Notes) == 0 {
		t.Error("no note explaining what last: measured back from")
	}
	// A time filter excludes untimestamped records; the count must be stated.
	if got.ExcludedNoTimestamp != 2 {
		t.Errorf("excluded_no_timestamp = %d, want 2", got.ExcludedNoTimestamp)
	}
}

// An empty result must explain itself rather than leaving a UI to render a
// blank table.
func TestQueryExplainsAnEmptyResult(t *testing.T) {
	srv := newTestServer(t)

	var got queryResponse
	do(t, srv, "POST", "/api/query", `{"filter":"status:>=999"}`, &got)

	if len(got.Rows) != 0 {
		t.Fatalf("got %d rows, want none", len(got.Rows))
	}
	if got.Explanation == nil {
		t.Fatal("no explanation for an empty result")
	}
	if got.Explanation.Text == "" {
		t.Error("the explanation is empty")
	}
}

// A filter error must arrive intact. A UI that shows "bad request" where the
// CLI shows a spelling suggestion is worse than the CLI at the moment the user
// most needs help.
func TestQueryErrorsCarryTheFullMessage(t *testing.T) {
	srv := newTestServer(t)

	tests := []struct {
		name string
		body string
		want []string
	}{
		{"unknown field", `{"filter":"sevrity:error"}`, []string{"unknown field", "fields present"}},
		{"syntax error", `{"filter":"msg:\"unterminated"}`, []string{"unterminated", "^"}},
		{"bad time", `{"filter":"last:banana"}`, []string{"last:15m"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got apiError
			code := do(t, srv, "POST", "/api/query", tt.body, &got)

			if code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", code)
			}
			for _, want := range tt.want {
				if !strings.Contains(got.Error, want) {
					t.Errorf("error %q does not contain %q", got.Error, want)
				}
			}
		})
	}
}

func TestQueryRejectsBothFilterAndSQL(t *testing.T) {
	srv := newTestServer(t)

	var got apiError
	code := do(t, srv, "POST", "/api/query", `{"filter":"level:error","sql":"SELECT 1"}`, &got)

	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
	if !strings.Contains(got.Error, "not both") {
		t.Errorf("error = %q", got.Error)
	}
}

func TestQueryAcceptsRawSQL(t *testing.T) {
	srv := newTestServer(t)

	var got queryResponse
	code := do(t, srv, "POST", "/api/query",
		`{"sql":"SELECT level, count(*) AS n FROM logs GROUP BY 1 ORDER BY 1"}`, &got)

	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(got.Rows) == 0 {
		t.Error("no rows from a raw SQL query")
	}
}

func TestHistogramEndpoint(t *testing.T) {
	srv := newTestServer(t)

	var got histogramResponse
	code := do(t, srv, "POST", "/api/histogram", `{"filter":"","buckets":4}`, &got)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}

	if len(got.Buckets) == 0 {
		t.Fatal("no buckets returned")
	}
	if got.IntervalMS <= 0 {
		t.Errorf("interval_ms = %d", got.IntervalMS)
	}

	// The counts must add up to the records that have a timestamp, and the
	// ones that do not must be reported rather than silently dropped.
	var summed int64
	for _, b := range got.Buckets {
		summed += b.Count
	}
	if summed != got.Total {
		t.Errorf("buckets sum to %d, total says %d", summed, got.Total)
	}
	if got.NoTimestamp != 2 {
		t.Errorf("no_timestamp = %d, want 2 — untimestamped records must be declared", got.NoTimestamp)
	}
	if summed+got.NoTimestamp != 7 {
		t.Errorf("%d bucketed + %d untimestamped = %d, want all 7 records accounted for",
			summed, got.NoTimestamp, summed+got.NoTimestamp)
	}
}

// The level breakdown is what colours the timeline, and it must sum to the
// bucket count or a stacked bar lies about its own height.
func TestHistogramLevelsSumToTheCount(t *testing.T) {
	srv := newTestServer(t)

	var got histogramResponse
	do(t, srv, "POST", "/api/histogram", `{"filter":"","buckets":10}`, &got)

	for _, b := range got.Buckets {
		var summed int64
		for _, n := range b.Levels {
			summed += n
		}
		if summed != b.Count {
			t.Errorf("bucket at %s: levels sum to %d, count is %d", b.Start, summed, b.Count)
		}
	}
}

func TestSourcesEndpoint(t *testing.T) {
	srv := newTestServer(t)

	var got []sourceInfo
	if code := do(t, srv, "GET", "/api/sources", "", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sources, want 2", len(got))
	}

	formats := map[string]bool{}
	for _, s := range got {
		formats[s.Format] = true
	}
	if !formats["jsonl"] || !formats["logfmt"] {
		t.Errorf("formats = %v, want both jsonl and logfmt", formats)
	}
}

// The API is served over loopback and is not for other origins. A page on
// another site must not be able to read somebody's production logs.
func TestNoCrossOriginAccessIsGranted(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest("GET", "/api/schema", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want no such header", got)
	}
}

// Binding anywhere reachable would publish the logs, and there is no
// authentication to fall back on.
func TestListenRefusesNonLoopback(t *testing.T) {
	tests := []struct {
		addr    string
		wantErr bool
	}{
		{"127.0.0.1:0", false},
		{"localhost:0", false},
		{"[::1]:0", false},
		{"0.0.0.0:0", true},
		{"192.168.1.5:0", true},
		{"example.com:0", true},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			srv := New(fixture(t), Options{Addr: tt.addr})

			ln, err := srv.Listen()
			if ln != nil {
				ln.Close()
			}

			if tt.wantErr && err == nil {
				t.Error("bound to a non-loopback address")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("refused a loopback address: %v", err)
			}
			if tt.wantErr && err != nil && !strings.Contains(err.Error(), "loopback") {
				t.Errorf("error does not explain the rule: %v", err)
			}
		})
	}
}

func TestServeShutsDownOnContextCancel(t *testing.T) {
	srv := New(fixture(t), Options{Addr: "127.0.0.1:0"})

	ln, err := srv.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	// Give the listener a moment, then ask it to stop.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the context was cancelled")
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	srv := newTestServer(t)

	body := `{"filter":"` + strings.Repeat("a", 2<<20) + `"}`
	code := do(t, srv, "POST", "/api/query", body, nil)

	if code == http.StatusOK {
		t.Error("a 2MB request body was accepted")
	}
}

func sameRow(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
