package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// tailClient is one open SSE connection against a live server.
type tailClient struct {
	events chan sseEvent
	cancel context.CancelFunc
	done   chan struct{}
}

type sseEvent struct {
	name string
	data []byte
}

// liveServer starts a real listener, because the tail endpoint streams: an
// httptest.ResponseRecorder buffers everything until the handler returns, and
// this handler is not supposed to return.
func liveServer(t *testing.T) (*Server, string, string) {
	t.Helper()

	sess, dir := fixtureDir(t)
	srv := New(sess, nil, Options{Addr: "127.0.0.1:0"})

	ln, err := srv.Listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx, ln) }()

	return srv, "http://" + ln.Addr().String(), dir
}

// openTail connects to /api/tail and decodes events onto a channel.
//
// The whole life of the connection sits inside the reading goroutine: it makes
// the request, owns the response body, and closes it on the way out. Doing the
// request out here and handing the body over would split ownership of a thing
// that has to stay open for the length of the test, which is how a leaked
// connection ends up holding a subscriber open on a server that is meant to
// have stopped polling.
func openTail(t *testing.T, base, filter string) *tailClient {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	url := base + "/api/tail?filter=" + strings.ReplaceAll(filter, " ", "%20")

	c := &tailClient{events: make(chan sseEvent, 64), cancel: cancel, done: make(chan struct{})}

	// Connecting is reported back here so the failure is raised on the test's
	// own goroutine. t.Fatal from anywhere else does not stop the test.
	ready := make(chan error, 1)

	go func() {
		defer close(c.done)

		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			ready <- fmt.Errorf("request: %w", err)
			return
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			ready <- fmt.Errorf("connect to %s: %w", url, err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			ready <- fmt.Errorf("tail returned %d: %s", resp.StatusCode, body)
			return
		}
		if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
			ready <- fmt.Errorf("content-type = %q, want text/event-stream", got)
			return
		}
		ready <- nil

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

		var name string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				select {
				case c.events <- sseEvent{name: name, data: []byte(strings.TrimPrefix(line, "data: "))}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	if err := <-ready; err != nil {
		cancel()
		<-c.done
		t.Fatal(err)
	}

	t.Cleanup(func() { cancel(); <-c.done })
	return c
}

// await waits for one event, failing if none arrives.
func (c *tailClient) await(t *testing.T, within time.Duration) sseEvent {
	t.Helper()
	select {
	case ev := <-c.events:
		return ev
	case <-time.After(within):
		t.Fatal("no event arrived on the live stream")
		return sseEvent{}
	}
}

// records waits for one records event and decodes it, failing on anything else.
func (c *tailClient) records(t *testing.T, within time.Duration) tailRecords {
	t.Helper()

	ev := c.await(t, within)
	if ev.name != "records" {
		t.Fatalf("expected a records event, got %s: %s", ev.name, ev.data)
	}

	var got tailRecords
	if err := json.Unmarshal(ev.data, &got); err != nil {
		t.Fatalf("decode records: %v", err)
	}
	return got
}

func appendLines(t *testing.T, dir, name string, lines ...string) {
	t.Helper()

	f, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s for append: %v", name, err)
	}
	defer f.Close()

	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatalf("append to %s: %v", name, err)
	}
}

// messages pulls the message column out of a records payload.
func messages(t *testing.T, rec tailRecords) []string {
	t.Helper()

	at := -1
	for i, c := range rec.Columns {
		if c == "message" {
			at = i
		}
	}
	if at < 0 {
		t.Fatalf("no message column in %v", rec.Columns)
	}

	out := make([]string, 0, len(rec.Rows))
	for _, row := range rec.Rows {
		out = append(out, fmt.Sprint(row[at]))
	}
	return out
}

// Nothing is being written, so nothing should be sent. A live tail that emits
// the existing records on connect would replay the whole file into the stream.
func TestTailIsSilentWhileNothingIsWritten(t *testing.T) {
	_, base, _ := liveServer(t)
	c := openTail(t, base, "")

	select {
	case ev := <-c.events:
		t.Fatalf("a quiet directory sent %s: %s", ev.name, ev.data)
	case <-time.After(3 * PollIntervalsWorth):
	}
}

// PollIntervalsWorth is long enough for several polls to have happened.
const PollIntervalsWorth = 500 * time.Millisecond

// The point of the endpoint: a line written after the page loaded arrives,
// once.
func TestTailDeliversAppendedRecords(t *testing.T) {
	_, base, dir := liveServer(t)
	c := openTail(t, base, "")

	appendLines(t, dir, "api.log",
		`{"ts":"2026-08-13T14:03:00Z","level":"error","msg":"live one","status":503,"trace_id":"b1"}`)

	got := c.records(t, 5*time.Second)
	if want := []string{"live one"}; !equalStrings(messages(t, got), want) {
		t.Fatalf("streamed %v, want %v", messages(t, got), want)
	}
	if got.Timezone != "UTC" {
		t.Errorf("timezone = %q, want UTC", got.Timezone)
	}
	// The running total is what lets the footer and the histogram know the
	// dataset moved underneath them.
	if got.Total != 8 {
		t.Errorf("total = %d, want 8", got.Total)
	}

	// Nothing further: a record must not be re-sent on the next poll.
	select {
	case ev := <-c.events:
		t.Fatalf("a second event arrived for one appended line: %s: %s", ev.name, ev.data)
	case <-time.After(3 * PollIntervalsWorth):
	}
}

// A live row must go through the same compiled filter as a queried one. A
// stream that showed records the same filter would exclude would disagree with
// the table it is feeding.
func TestTailAppliesTheFilter(t *testing.T) {
	_, base, dir := liveServer(t)
	c := openTail(t, base, "level:error")

	appendLines(t, dir, "api.log",
		`{"ts":"2026-08-13T14:04:00Z","level":"info","msg":"ignored","status":200,"trace_id":"c1"}`,
		`{"ts":"2026-08-13T14:04:01Z","level":"error","msg":"kept","status":500,"trace_id":"c2"}`)

	got := c.records(t, 5*time.Second)
	if want := []string{"kept"}; !equalStrings(messages(t, got), want) {
		t.Fatalf("streamed %v, want %v", messages(t, got), want)
	}
}

// Two open tabs must each see the record exactly once.
//
// This is why there is one follower per server rather than one per connection.
// Two followers each keep their own idea of where they have read to and each
// rewinds to re-read the last record, so they would delete and reinsert each
// other's rows: one stream would show duplicates and the other would miss
// lines entirely.
func TestTailServesTwoClientsFromOneFollower(t *testing.T) {
	_, base, dir := liveServer(t)

	first := openTail(t, base, "")
	second := openTail(t, base, "")

	appendLines(t, dir, "api.log",
		`{"ts":"2026-08-13T14:05:00Z","level":"warn","msg":"shared","status":429,"trace_id":"d1"}`)

	var wg sync.WaitGroup
	for _, c := range []*tailClient{first, second} {
		wg.Add(1)
		go func(c *tailClient) {
			defer wg.Done()
			got := c.records(t, 5*time.Second)
			if want := []string{"shared"}; !equalStrings(messages(t, got), want) {
				t.Errorf("streamed %v, want %v", messages(t, got), want)
			}
		}(c)
	}
	wg.Wait()

	// And neither gets it a second time.
	for i, c := range []*tailClient{first, second} {
		select {
		case ev := <-c.events:
			t.Fatalf("client %d got a duplicate: %s: %s", i, ev.name, ev.data)
		case <-time.After(3 * PollIntervalsWorth):
		}
	}
}

// A file that appears after the page loaded must be picked up. A service that
// starts logging during an incident is exactly when you least want to be told
// to reload.
func TestTailPicksUpANewFile(t *testing.T) {
	_, base, dir := liveServer(t)
	c := openTail(t, base, "")

	line := `{"ts":"2026-08-13T14:06:00Z","level":"error","msg":"from a new file","status":500,"trace_id":"e1"}`
	if err := os.WriteFile(filepath.Join(dir, "worker.log"), []byte(line+"\n"), 0o644); err != nil {
		t.Fatalf("write new file: %v", err)
	}

	got := c.records(t, 5*time.Second)
	if want := []string{"from a new file"}; !equalStrings(messages(t, got), want) {
		t.Fatalf("streamed %v, want %v", messages(t, got), want)
	}
}

// A live record must arrive with its promoted columns filled in, or a filter
// like status:>=500 would silently stop matching the incident being watched.
func TestTailRecordsHavePromotedColumns(t *testing.T) {
	_, base, dir := liveServer(t)
	c := openTail(t, base, "status:>=500")

	appendLines(t, dir, "api.log",
		`{"ts":"2026-08-13T14:07:00Z","level":"error","msg":"promoted","status":503,"trace_id":"f1"}`)

	got := c.records(t, 5*time.Second)
	if want := []string{"promoted"}; !equalStrings(messages(t, got), want) {
		t.Fatalf("streamed %v, want %v", messages(t, got), want)
	}
}

// A filter typo must fail the connection with the CLI's error, not open a
// stream that connects and then shows nothing forever.
func TestTailRejectsABadFilterBeforeStreaming(t *testing.T) {
	srv := newTestServer(t)

	var got apiError
	code := do(t, srv, "GET", "/api/tail?filter=levl:error", "", &got)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(got.Error, "level") {
		t.Errorf("error %q does not suggest the field the user meant", got.Error)
	}
}

// Nobody watching means nothing polling. This is what keeps the no-daemon
// promise literal: an idle `loupe serve` must not be re-statting the log
// directory forever.
func TestTailStopsPollingWhenTheLastClientLeaves(t *testing.T) {
	srv, base, _ := liveServer(t)

	c := openTail(t, base, "")
	waitFor(t, func() bool { return srv.tail.polling() },
		"the poll loop did not start when a client connected")

	c.cancel()
	<-c.done

	waitFor(t, func() bool { return !srv.tail.polling() },
		"the poll loop kept running after the last client left")
}

// polling reports whether the hub currently has a poll loop.
func (h *tailHub) polling() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.running
}

func waitFor(t *testing.T, condition func() bool, message string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(message)
}

func equalStrings(a, b []string) bool {
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
