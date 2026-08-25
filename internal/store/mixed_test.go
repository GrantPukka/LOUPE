package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/GrantPukka/loupe/internal/schema"
)

// mergedLines builds a file holding several formats at once, the way a
// collector or `cat *.log > combined.log` does.
func mergedLines(n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out,
			fmt.Sprintf(`{"ts":"2026-08-13T14:%02d:00Z","level":"info","msg":"request completed","status":200}`, i%60),
			fmt.Sprintf(`ts=2026-08-13T14:%02d:01Z level=error msg="upstream timeout" service=checkout`, i%60),
			fmt.Sprintf(`10.0.3.48 - - [13/Aug/2026:14:%02d:02 +0000] "GET /healthz HTTP/1.1" 200 512 "-" "-"`, i%60),
		)
	}
	return out
}

// The headline failure: one file, many formats, and a file-level parser choice
// that left most of it off the timeline. 84.5% unparsed on the corpus that
// prompted this.
func TestMergedFileIsReadPerLine(t *testing.T) {
	dir := logDir(t, mergedLines(20)...)
	cached := openCached(t, dir, t.TempDir(), CacheOptions{})

	stats := cached.Load.Stats
	if stats.Unparsed != 0 {
		t.Errorf("Unparsed = %d of %d, want 0 — every line here is a known format",
			stats.Unparsed, stats.Records)
	}
	if stats.NoTimestamp != 0 {
		t.Errorf("NoTimestamp = %d, want 0 — a record with no timestamp is off the timeline", stats.NoTimestamp)
	}

	// Each record has to carry the format that actually read it, or a merged
	// file cannot be broken down by format at all.
	res, err := cached.DB.QueryResult(context.Background(), 0,
		`SELECT format, count(*) FROM logs GROUP BY 1 ORDER BY 1`)
	if err != nil {
		t.Fatalf("group by format: %v", err)
	}

	got := map[string]bool{}
	for _, row := range res.Rows {
		if name, ok := row[0].(string); ok {
			got[name] = true
		}
	}
	for _, want := range []string{"jsonl", "logfmt", "nginx"} {
		if !got[want] {
			t.Errorf("no records recorded as %s; formats found: %v", want, got)
		}
	}
}

// The fields carried by one format inside a merged file must still earn
// columns. Judged across the whole file each covers about a third of it, which
// is under the promotion bar — and `loupe top` and `stats … by <field>` are
// unusable without them.
func TestMergedFilePromotesPerFormatFields(t *testing.T) {
	dir := logDir(t, mergedLines(20)...)
	cached := openCached(t, dir, t.TempDir(), CacheOptions{})

	promotions, _, err := cached.DB.InferAndPromote(context.Background(), schema.Options{})
	if err != nil {
		t.Fatalf("InferAndPromote: %v", err)
	}

	got := map[string]bool{}
	for _, p := range promotions {
		got[p.Field] = true
	}
	for _, want := range []string{"status", "service"} {
		if !got[want] {
			t.Errorf("%s was not promoted; promoted: %v", want, keys(got))
		}
	}
}

// A file that really is one format must keep using that format's parser, so the
// common case pays nothing for this.
func TestUniformFileKeepsItsDetectedParser(t *testing.T) {
	var lines []string
	for i := 0; i < 60; i++ {
		lines = append(lines,
			fmt.Sprintf(`{"ts":"2026-08-13T14:%02d:00Z","level":"info","msg":"a","status":200}`, i%60))
	}
	// A scattering of damage must not be enough to call the file mixed.
	lines = append(lines, "truncated {", "not json at all")

	dir := logDir(t, lines...)
	cached := openCached(t, dir, t.TempDir(), CacheOptions{})

	for _, r := range cached.Load.Results {
		if r.Source.Format != "jsonl" {
			t.Errorf("format = %q, want jsonl", r.Source.Format)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A tiny sample cannot support a verdict about a file's formats.
func TestShortFileIsNotDeclaredMixed(t *testing.T) {
	dir := logDir(t,
		`{"ts":"2026-08-13T14:00:00Z","level":"info","msg":"a"}`,
		`{"ts":"2026-08-13T14:00:01Z","level":"info","msg":"b"}`,
		`not json at all`,
	)
	cached := openCached(t, dir, t.TempDir(), CacheOptions{})

	for _, r := range cached.Load.Results {
		if strings.Contains(r.Source.Format, "mixed") {
			t.Errorf("a three-line file was declared mixed on no evidence: %q", r.Source.Format)
		}
	}
}
