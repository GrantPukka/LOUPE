package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GrantPukka/loupe/internal/store"
)

// pipeStdin replaces os.Stdin with a pipe carrying body, for the length of the
// test. The session reads the real os.Stdin, so exercising it honestly means
// giving the process a real one.
func pipeStdin(t *testing.T, body string) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	original := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = original
		r.Close()
	})

	go func() {
		defer w.Close()
		_, _ = w.WriteString(body)
	}()
}

const streamLines = `{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"piped one","status":200}
{"ts":"2026-08-13T14:01:00Z","level":"error","msg":"piped two","status":500}
{"ts":"2026-08-13T14:02:00Z","level":"warn","msg":"piped three","status":429}
`

// openStdin opens a streaming session and reads it to the end.
//
// Opening no longer reads a stream — that is the whole point of streaming —
// so a test that wants the finished totals has to drain it first.
func openStdin(t *testing.T, paths ...string) *Session {
	t.Helper()

	sess := openStream(t, paths...)
	if err := sess.Stream(context.Background(), "", func(store.Result) error {
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return sess
}

// The documented pipeline: kubectl logs api | loupe.
func TestSessionReadsAPipedStream(t *testing.T) {
	pipeStdin(t, streamLines)
	sess := openStdin(t)

	if sess.Load.Stats.Records != 3 {
		t.Fatalf("ingested %d records, want 3", sess.Load.Stats.Records)
	}
	if !sess.HasStream() {
		t.Error("a session over stdin does not report having a stream")
	}

	// The whole stream must arrive. Format detection samples the source before
	// the ingest reads it, and on a stream that sampling used to consume the
	// first two hundred lines and lose them.
	got, err := sess.Count(context.Background(), plan(t, sess, ""))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 3 {
		t.Errorf("counted %d records, want 3", got)
	}
}

// The filter path is the same one every other source uses.
func TestStreamRecordsAreFilterable(t *testing.T) {
	pipeStdin(t, streamLines)
	sess := openStdin(t)

	got, err := sess.Count(context.Background(), plan(t, sess, "level:error"))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 1 {
		t.Errorf("level:error matched %d records, want 1", got)
	}

	// Promotion runs on a stream like anywhere else, so a JSON field is a real
	// column and comparisons on it are numeric.
	got, err = sess.Count(context.Background(), plan(t, sess, "status:>=500"))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 1 {
		t.Errorf("status:>=500 matched %d records, want 1", got)
	}
}

// A stream and a directory land on one timeline, which is the tool's premise
// applied to a pipe.
func TestStreamComposesWithAPath(t *testing.T) {
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "app.log"),
		[]byte(`{"ts":"2026-08-13T14:03:00Z","level":"info","msg":"from a file"}`+"\n"), 0o644)
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	pipeStdin(t, streamLines)
	sess := openStdin(t, dir, StdinPath)

	if sess.Load.Stats.Records != 4 {
		t.Fatalf("ingested %d records, want 4", sess.Load.Stats.Records)
	}

	res, err := sess.Records(context.Background(), plan(t, sess, ""),
		RecordQuery{Sort: SortTime, Columns: "source, message"})
	if err != nil {
		t.Fatalf("records: %v", err)
	}

	sources := map[string]int{}
	for _, row := range res.Rows {
		name, _ := row[0].(string)
		sources[name]++
	}
	if sources["stdin"] != 3 {
		t.Errorf("stdin contributed %d records, want 3 (%v)", sources["stdin"], sources)
	}
	if sources["app"] != 1 {
		t.Errorf("the file contributed %d records, want 1 (%v)", sources["app"], sources)
	}
}

// An empty pipe is an ordinary outcome — a pod that has not logged yet — and
// must not be an error.
func TestEmptyStreamIsNotAnError(t *testing.T) {
	pipeStdin(t, "")
	sess := openStdin(t)

	if sess.Load.Stats.Records != 0 {
		t.Errorf("an empty pipe produced %d records", sess.Load.Stats.Records)
	}
}

// A stream cannot be re-read, so it is never cached — and the reason has to be
// on screen, because the cost of re-reading is paid every run.
func TestStreamIsNotCachedAndSaysWhy(t *testing.T) {
	pipeStdin(t, streamLines)

	sess, err := Open(context.Background(), Options{
		Paths:    []string{StdinPath},
		Location: time.UTC,
		CacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close()

	if sess.CacheHit {
		t.Error("a stream reported a cache hit")
	}
	if sess.CachePath != "" {
		t.Errorf("a stream was cached to %q", sess.CachePath)
	}
	if !strings.Contains(sess.CacheReason, "stream") {
		t.Errorf("cache reason %q does not explain that a stream cannot be cached",
			sess.CacheReason)
	}
}

// A record split across the end of the stream is still a record. The writer
// closing mid-line is what a killed pod looks like.
func TestStreamClosedMidRecord(t *testing.T) {
	pipeStdin(t, streamLines+`{"ts":"2026-08-13T14:04:00Z","level":"error","msg":"truncated`)
	sess := openStdin(t)

	// Four records, not three: the half-written line is kept as unparsed
	// rather than dropped, because a tool that silently discards the last line
	// of a stream is a tool that hides the thing that killed the process.
	if sess.Load.Stats.Records != 4 {
		t.Errorf("ingested %d records, want 4 including the truncated one",
			sess.Load.Stats.Records)
	}

	got, err := sess.Count(context.Background(), plan(t, sess, "parsed:false"))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 1 {
		t.Errorf("%d unparsed records, want the truncated line to be one", got)
	}
}

// The follower must never be handed a stream: there is nothing to re-resolve
// and no offset to resume from.
//
// The assertion is about the stream's records specifically, not the total. A
// file alongside it is re-readable and the follower is entitled to look at it;
// the pipe is not, and re-reading one would either fail or duplicate whatever
// the pipe happened to still hold.
func TestFollowerSkipsAStream(t *testing.T) {
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "app.log"),
		[]byte(`{"ts":"2026-08-13T14:03:00Z","level":"info","msg":"on disk"}`+"\n"), 0o644)
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	pipeStdin(t, streamLines)
	sess := openStream(t, dir, StdinPath)

	if err := sess.Stream(context.Background(), "", func(store.Result) error {
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	before := countStdinRecords(t, sess)
	if before != 3 {
		t.Fatalf("the stream contributed %d records, want 3", before)
	}

	follower, err := sess.Follower(context.Background())
	if err != nil {
		t.Fatalf("Follower: %v", err)
	}
	if _, err := follower.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	if after := countStdinRecords(t, sess); after != before {
		t.Errorf("the stream held %d records before the poll and %d after; it was re-read",
			before, after)
	}
}

func countStdinRecords(t *testing.T, sess *Session) int64 {
	t.Helper()

	var n int64
	err := sess.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM logs WHERE source = 'stdin'`).Scan(&n)
	if err != nil {
		t.Fatalf("count stdin records: %v", err)
	}
	return n
}

// openStream opens a streaming session without reading it, which is what a
// caller that wants to drive the stream itself does.
func openStream(t *testing.T, paths ...string) *Session {
	t.Helper()

	if len(paths) == 0 {
		paths = []string{StdinPath}
	}
	sess, err := Open(context.Background(), Options{Paths: paths, Location: time.UTC})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { sess.Close() })

	if !sess.Streaming() {
		t.Fatal("a session over stdin is not streaming")
	}
	return sess
}

// collect drives a streaming session and returns every message emitted, in
// order, with how many batches it took.
func collect(t *testing.T, sess *Session, filter string) ([]string, int) {
	t.Helper()

	var messages []string
	batches := 0

	err := sess.Stream(context.Background(), filter, func(res store.Result) error {
		batches++
		at := -1
		for i, c := range res.Columns {
			if c == "message" {
				at = i
			}
		}
		if at < 0 {
			t.Fatalf("no message column in %v", res.Columns)
		}
		for _, row := range res.Rows {
			m, _ := row[at].(string)
			messages = append(messages, m)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return messages, batches
}

func TestStreamEmitsEveryRecord(t *testing.T) {
	pipeStdin(t, streamLines)
	sess := openStream(t)

	got, _ := collect(t, sess, "")
	want := []string{"piped one", "piped two", "piped three"}

	if len(got) != len(want) {
		t.Fatalf("emitted %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// Every record exactly once. A batch boundary that overlapped or skipped would
// double-count or lose records, and neither shows up without counting.
func TestStreamEmitsEachRecordOnce(t *testing.T) {
	var body strings.Builder
	const count = 1200
	for i := 0; i < count; i++ {
		fmt.Fprintf(&body, `{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"record %d"}`+"\n", i)
	}

	pipeStdin(t, body.String())
	sess := openStream(t)

	got, batches := collect(t, sess, "")
	if len(got) != count {
		t.Fatalf("emitted %d records, want %d", len(got), count)
	}
	if batches < 2 {
		t.Fatalf("%d records arrived in %d batch(es); this is not testing batching", count, batches)
	}

	seen := map[string]bool{}
	for _, m := range got {
		if seen[m] {
			t.Fatalf("%q was emitted twice", m)
		}
		seen[m] = true
	}
}

// The filter runs through the compiled DSL, exactly as it does anywhere else.
func TestStreamAppliesTheFilter(t *testing.T) {
	pipeStdin(t, streamLines)
	sess := openStream(t)

	got, _ := collect(t, sess, "level:error")
	if len(got) != 1 || got[0] != "piped two" {
		t.Errorf("emitted %v, want just the error record", got)
	}
}

// A field present only in the JSON bag is still filterable. Promotion is what
// streaming gives up, not queryability.
func TestStreamFiltersOnBagFields(t *testing.T) {
	pipeStdin(t, streamLines)
	sess := openStream(t)

	got, _ := collect(t, sess, "status:>=500")
	if len(got) != 1 || got[0] != "piped two" {
		t.Errorf("status:>=500 emitted %v, want the 500 record", got)
	}
}

// A typo is an error, not an empty stream that looks like a quiet pod.
func TestStreamRejectsAnUnknownField(t *testing.T) {
	pipeStdin(t, streamLines)
	sess := openStream(t)

	err := sess.Stream(context.Background(), "stats:>=500", func(store.Result) error {
		return nil
	})
	if err == nil {
		t.Fatal("an unknown field streamed without error")
	}
	if !strings.Contains(err.Error(), "status") {
		t.Errorf("error does not suggest the field meant: %v", err)
	}
	if !strings.Contains(err.Error(), "arrived so far") {
		t.Errorf("error does not explain what a stream resolved against: %v", err)
	}
}

// A file listed alongside a stream must be read. Load takes sources in order,
// and a pipe that never ends would otherwise starve everything behind it.
func TestStreamReadsFiniteSourcesFirst(t *testing.T) {
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "app.log"),
		[]byte(`{"ts":"2026-08-13T13:00:00Z","level":"info","msg":"from the file"}`+"\n"), 0o644)
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	pipeStdin(t, streamLines)
	sess := openStream(t, dir, StdinPath)

	got, _ := collect(t, sess, "")
	if len(got) == 0 || got[0] != "from the file" {
		t.Fatalf("emitted %v, want the file's record first", got)
	}
	if len(got) != 4 {
		t.Errorf("emitted %d records, want 4", len(got))
	}
}

// Cancelling is how a live tail ends. It must stop rather than run on, and it
// must say it stopped rather than report a clean finish.
func TestStreamStopsWhenCancelled(t *testing.T) {
	pipeStdin(t, streamLines)
	sess := openStream(t)

	ctx, cancel := context.WithCancel(context.Background())

	err := sess.Stream(ctx, "", func(store.Result) error {
		cancel()
		return nil
	})
	if err == nil {
		t.Fatal("a cancelled stream returned no error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

// An empty pipe emits nothing and is not an error: a pod that has not logged
// yet is the ordinary case.
func TestStreamOnAnEmptyPipe(t *testing.T) {
	pipeStdin(t, "")
	sess := openStream(t)

	got, batches := collect(t, sess, "")
	if len(got) != 0 || batches != 0 {
		t.Errorf("an empty pipe emitted %d records in %d batches", len(got), batches)
	}
}
