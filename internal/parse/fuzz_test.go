package parse

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Fuzzing the parsers is worth more here than in most projects.
//
// Every parser is fed bytes chosen by somebody else, CLAUDE.md promises that a
// malformed line never aborts a file, and the Parser interface is the
// contribution surface — a parser arriving by PR from a stranger is precisely
// the code least covered by anyone's intuition about what a log line looks
// like.
//
// Go runs a fuzz target's seed corpus as an ordinary test under `go test`, so
// these carry their regression value in CI without a longer run.

// seedFromFixtures adds a line from each checked-in golden fixture, so the
// corpus starts on the real formats rather than on random bytes.
func seedFromFixtures(f *testing.F) {
	f.Helper()

	paths, err := filepath.Glob(filepath.Join("testdata", "*", "*.log"))
	if err != nil {
		return
	}

	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for i, line := range bytes.Split(body, []byte("\n")) {
			// A handful per fixture: the corpus wants breadth of shape, not
			// every line of every file.
			if i >= 5 {
				break
			}
			if len(bytes.TrimSpace(line)) > 0 {
				f.Add(line)
			}
		}
	}
}

// FuzzParsers runs every registered parser over one line.
//
// Every parser sees every input rather than the fuzzer picking one: a line
// shaped like logfmt is exactly the input most likely to break the JSON parser,
// and detection means any parser can be handed any format in practice.
func FuzzParsers(f *testing.F) {
	seedFromFixtures(f)

	// The shapes most likely to break something, kept explicit so they survive
	// a corpus reset.
	for _, seed := range []string{
		"",
		" ",
		"\x00",
		"{",
		`{"ts":`,
		`{"ts":"2026-08-13T14:00:00Z","level":"error","msg":"x"}`,
		"ts=2026-08-13T14:00:00Z level=info msg=x",
		`10.0.0.1 - - [13/Aug/2026:14:00:00 +0000] "GET / HTTP/1.1" 200 1`,
		"<14>1 2026-08-13T14:00:00Z host app 1 - - x",
		"2026-08-13 14:00:00.000 [main] ERROR c.a.X - boom",
		"2026-08-13 14:00:00.000 UTC [1] LOG:  statement: SELECT 1",
		"\ufeff{}",
		"level=\"unterminated",
	} {
		f.Add([]byte(seed))
	}

	parsers := All()

	f.Fuzz(func(t *testing.T, line []byte) {
		// The reader hands every parser a slice of its own buffer and keeps a
		// copy as `raw`. A parser that writes into that slice corrupts the raw
		// text of the record it just produced — invisible in normal use and
		// impossible to explain afterwards.
		original := append([]byte(nil), line...)

		for _, p := range parsers {
			rec, err := p.Parse(line)

			if !bytes.Equal(line, original) {
				t.Fatalf("%s.Parse mutated its input\n got: %q\nwant: %q",
					p.Name(), line, original)
			}
			if err != nil {
				continue
			}

			// A timestamp with no location panics the moment anything compares
			// or formats it, which happens far from here.
			if !rec.Timestamp.IsZero() && rec.Timestamp.Location() == nil {
				t.Fatalf("%s.Parse returned a timestamp with no location for %q",
					p.Name(), line)
			}
			// A parser is a pure function of one line; a timestamp cannot
			// legitimately land beyond the range time.Time can render.
			if !rec.Timestamp.IsZero() {
				if y := rec.Timestamp.Year(); y < 0 || y > 9999 {
					t.Fatalf("%s.Parse returned year %d for %q", p.Name(), y, line)
				}
			}
		}
	})
}

// FuzzParseIsDeterministic pins that a parser is a pure function of its line.
//
// Anything that reads a clock, a map iteration order, or package state would
// make two ingests of the same file disagree, and the cache would then serve
// whichever answer happened to be recorded first.
func FuzzParseIsDeterministic(f *testing.F) {
	seedFromFixtures(f)
	f.Add([]byte(`{"ts":"2026-08-13T14:00:00Z","a":1,"b":2,"msg":"x"}`))

	parsers := All()

	f.Fuzz(func(t *testing.T, line []byte) {
		for _, p := range parsers {
			first, err1 := p.Parse(line)
			second, err2 := p.Parse(line)

			if (err1 == nil) != (err2 == nil) {
				t.Fatalf("%s.Parse disagreed with itself on %q: %v then %v",
					p.Name(), line, err1, err2)
			}
			if err1 != nil {
				continue
			}
			if !first.Timestamp.Equal(second.Timestamp) {
				t.Fatalf("%s.Parse gave two timestamps for %q: %s then %s",
					p.Name(), line, first.Timestamp, second.Timestamp)
			}
			if first.Level != second.Level || first.Message != second.Message {
				t.Fatalf("%s.Parse gave two records for %q", p.Name(), line)
			}
			if len(first.Fields) != len(second.Fields) {
				t.Fatalf("%s.Parse gave %d fields then %d for %q",
					p.Name(), len(first.Fields), len(second.Fields), line)
			}
		}
	})
}

// FuzzDetect checks the confidence contract every parser's doc comment makes.
//
// A parser returning 1.5, or NaN, does not merely score itself too highly: it
// wins detection against every other format for every file, and the failure
// looks like "loupe reads my logs wrong" rather than like a bug in one parser.
func FuzzDetect(f *testing.F) {
	seedFromFixtures(f)
	f.Add([]byte("{}\n{}\n"))
	f.Add([]byte(""))
	f.Add([]byte("\n\n\n"))

	parsers := All()

	f.Fuzz(func(t *testing.T, body []byte) {
		sample := bytes.Split(body, []byte("\n"))

		for _, p := range parsers {
			got := p.Detect(sample)

			// NaN fails every comparison, so it is caught by its own inequality
			// rather than by the range check below.
			if got != got {
				t.Fatalf("%s.Detect returned NaN", p.Name())
			}
			if got < 0 || got > 1 {
				t.Fatalf("%s.Detect returned %v, outside 0.0-1.0", p.Name(), got)
			}
		}

		// Detect over the registry must also settle on something sane.
		det := Detect(sample)
		if det.Confidence < 0 || det.Confidence > 1 {
			t.Fatalf("Detect returned confidence %v, outside 0.0-1.0", det.Confidence)
		}
	})
}

// FuzzReadAll drives the whole reader, including the continuation handling and
// the resume bookkeeping that EC001 depends on.
//
// The stats are not decoration: the status line reports them as claims about
// the data, and `before` is what makes an incremental re-read add up. A count
// that drifts under strange input is a wrong claim, which is worse than a
// crash because nobody notices.
func FuzzReadAll(f *testing.F) {
	f.Add("")
	f.Add("\n")
	f.Add("{}\n{}\n")
	f.Add(`{"ts":"2026-08-13T14:00:00Z","msg":"a"}` + "\n\tat Foo.bar(Foo.java:1)\n")
	f.Add("2026-08-13 14:00:00.000 [main] ERROR c.a.X - boom\n\tat X.y(X.java:2)\n")
	f.Add("a\n\n\nb\n")

	parsers := All()

	f.Fuzz(func(t *testing.T, body string) {
		for _, parser := range parsers {
			name := parser.Name()

			var seen int64
			stats, tail, err := ReadAll(bytes.NewReader([]byte(body)),
				ReaderOptions{Parser: parser, Loc: time.UTC},
				func(Entry) error {
					seen++
					return nil
				})
			if err != nil {
				t.Fatalf("ReadAll(%q) with %s: %v", body, name, err)
			}

			if stats.Records != seen {
				t.Fatalf("%s: reported %d records but emitted %d for %q",
					name, stats.Records, seen, body)
			}
			if stats.Unparsed > stats.Records {
				t.Fatalf("%s: %d unparsed of %d records for %q",
					name, stats.Unparsed, stats.Records, body)
			}
			if stats.NoTimestamp > stats.Records {
				t.Fatalf("%s: %d without a timestamp of %d records for %q",
					name, stats.NoTimestamp, stats.Records, body)
			}
			// The tail is where a resumed read restarts. An offset past the
			// end would skip bytes that were never read, which is the silent
			// data loss this project refuses.
			if tail.Offset < 0 || tail.Offset > int64(len(body)) {
				t.Fatalf("%s: resume offset %d outside 0..%d for %q",
					name, tail.Offset, len(body), body)
			}
			if tail.Line < 0 {
				t.Fatalf("%s: negative resume line %d for %q", name, tail.Line, body)
			}
		}
	})
}
