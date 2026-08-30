# Contributing

Thanks for looking. The most valuable contribution to this project is **a parser for a log
format you actually have** — that is the whole reason this project can be useful to people
whose logs look nothing like mine.

A parser is one file, roughly 100 lines, plus a fixture. The walkthrough below should take
about twenty minutes.

---

## Setup

```bash
git clone https://github.com/GrantPukka/loupe && cd loupe
go build ./cmd/loupe          # requires Go 1.24+, CGO enabled
go test ./...
./loupe internal/parse/testdata/jsonl/sample.log
```

CGO is required because we link DuckDB. On Linux you need a C toolchain
(`build-essential`); on macOS, the Xcode command line tools.

For frontend work: `cd web && npm install && npm run dev`, which proxies the API to a
`loupe serve` running on the default port.

There is also a browser test suite. It drives the embedded UI against a real binary and a
real DuckDB ingest — nothing is mocked, for the same reason the Go tests do not mock the
store:

```bash
make web && make build      # the tests drive the binary you just built
make e2e                    # or: cd web && npm run test:e2e
```

It generates its own fixtures from `cmd/blaster` on a fixed seed, serves them on port 7799,
and makes no outbound connections. If you change the UI, run it before opening the PR.

---

## Adding a parser — the walkthrough

Say you want to support Nginx error logs.

### 1. Add a fixture

Create `internal/parse/testdata/nginx-error/sample.log` with 20–50 real-looking lines.
**Make it messy on purpose:** include a blank line, a truncated final line, and at least one
malformed record. Scrub any real hostnames, IPs, or tokens.

This is not a style request — `TestFixturesAreMessy` checks it, and `TestFixturesAreSmall`
enforces the size limit. A tidy fixture proves the parser works on input that never occurs.

### 2. Write the parser

Create `internal/parse/nginx_error.go`. Copy the shape of `logfmt.go`, which is the clearest
example:

```go
package parse

func init() { Register(&nginxErrorParser{}) }

type nginxErrorParser struct{}

func (p *nginxErrorParser) Name() string { return "nginx-error" }

// Detect gets the first ~200 lines of a source and returns 0.0–1.0 confidence.
// Be conservative: it is much better to return 0.3 and lose a coin-flip than to
// return 0.9 and hijack a format that belongs to another parser.
func (p *nginxErrorParser) Detect(sample [][]byte) float64 {
    var matched int
    for _, line := range sample {
        if nginxErrorRe.Match(line) {
            matched++
        }
    }
    if len(sample) == 0 {
        return 0
    }
    return float64(matched) / float64(len(sample))
}

func (p *nginxErrorParser) Parse(line []byte) (Record, error) {
    m := nginxErrorRe.FindSubmatch(line)
    if m == nil {
        return Record{}, ErrNoMatch
    }

    rec := Record{Fields: make(map[string]any, 4)}

    // Use ParseTime, not time.Parse. It tries every known layout, handles
    // epoch numbers, and tells you whether the text carried its own zone.
    //
    // A missing or unparseable timestamp is NOT a fatal error: leave
    // Timestamp at its zero value and the record stays ingested and
    // queryable through ts:none.
    if ts, zoned, ok := ParseTime(string(m[1]), time.UTC); ok {
        rec.Timestamp, rec.TimestampZoned = ts, zoned
    }

    rec.Level = NormaliseLevel(string(m[2]))
    rec.Message = string(m[3])
    rec.Fields["client"] = string(m[4])
    rec.Fields["request"] = string(m[5])

    return rec, nil
}
```

Four rules that cover most review comments:

- **Never return a fatal error for one bad line.** Return `ErrNoMatch` and the pipeline will
  fall back gracefully. One corrupt line must not abort a 4GB file.
- **Set `TimestampZoned` honestly.** It says whether the *line* carried a zone. `false` means
  the time depends on an assumption, which loupe then discloses in the status line and in
  every handoff. Hard-coding `true` for a format that carries no offset silently converts an
  assumption into a claim — the worst bug you can introduce here. Taking it from `ParseTime`
  gets this right for free.
- **Normalise levels** through `NormaliseLevel` so `WARN`, `warning`, and `W` all become
  `warn`. Cross-format filtering depends on this.
- **Put unrecognised key/values in `Fields`, don't drop them.** Anything you drop is
  invisible forever.

### 3. Generate and check the golden file

```bash
go test ./internal/parse -run TestParsers -update
git diff testdata/nginx-error/    # read this carefully — it is your actual test
go test ./...
```

### 4. Open the PR

Include a couple of sample lines in the description and say where the format comes from
(which tool, which version, which config). One commit, `feat(parse): add nginx error log
parser`.

That is the whole process. No other file in the repository needs to change.

---

## If you change what gets ingested, bump `IngestVersion`

`internal/store/cache.go` has a constant:

```go
const IngestVersion = 4
```

It is hashed into every cache key. Bump it whenever a change would make a
cached database disagree with a fresh ingest of the same files:

- the `logs` table schema
- any parser's output, including a new field or a changed field name
- level normalisation
- timestamp parsing or the assumed-timezone rules
- schema inference or the promotion rules

Forgetting is the easiest way to introduce a subtle bug in this codebase.
Nothing breaks on your machine — you will have re-ingested while developing —
but every existing user keeps silently reading records produced by the code you
just fixed, with no warning and no way to tell.

`go test ./internal/store -run TestStaleIngestVersionIsRejected` covers the
mechanism, not your bump. Only you can do that part.

---

## Other good contributions

- **Sources** — new places to read bytes from (`internal/source/`). Same shape: one file, one
  interface, one fixture.
- **Timestamp formats** — `internal/parse/timestamp.go` holds the layouts tried during
  inference. Adding one is a two-line PR with a test, and genuinely useful. If the layout
  carries no timezone, add it to `zonelessLayouts` too, or times from that format will be
  reported as known when they are actually assumed.
- **Performance** — bring a benchmark. `go test -bench` before and after, both numbers in the
  PR description. For end-to-end claims rather than one function, [docs/BENCHMARKING.md](docs/BENCHMARKING.md)
  sets out how to build a corpus and a ground truth that can be trusted, and why the obvious
  way to do it produces a number that is quietly wrong.
- **Bug reports with a fixture** — a 20-line sample that reproduces the problem is worth more
  than a long description.

---

## What will get declined

Not because the ideas are bad, but because this project stays finishable by refusing them.
See the non-goals in `ARCHITECTURE.md`.

- Alerting, monitoring, or notifications
- Authentication, user accounts, multi-tenancy
- A persistent ingestion daemon or log shipping
- Metrics or trace support
- Anything requiring an external service to run
- Saved searches, dashboards, or a settings page in the UI

If you are unsure whether something fits, open an issue before writing code. I would much
rather have that conversation early than decline a finished PR.

---

## Style and process

- `gofmt`, `go vet`, and `golangci-lint run` must pass. `make check` runs all
  three plus the tests. CI runs the same things on Linux and macOS, with `-race`,
  plus the browser suite — running `make check` locally first just saves you the
  round trip.
- Conventional commits (`feat:`, `fix:`, `docs:`, ...), imperative mood, under 72 characters.
- New behaviour needs a test.
- Discuss new third-party dependencies in an issue first.
- Be decent to each other in issues and reviews.

---

## Seed issues to label `good first issue`

*(Maintainer note — create these before announcing anywhere. Each should link to
`logfmt.go` as the template and include a sample of the target format. Check the
item is still open before filing it: an earlier version of this list shipped
with seven entries that were already built, which wastes the time of exactly the
people you least want to waste.)*

**Parsers** — one file, one fixture, no other file changes:

1. Nginx error log (the access log is done; the error log is a different format)
2. Apache error log
3. Python `logging` default format
4. Rails production log
5. Kubernetes CRI log format
6. Docker JSON file logging driver
7. AWS CloudTrail JSON
8. Caddy JSON access logs
9. systemd `journalctl -o json`
10. HAProxy HTTP log
11. MySQL slow query log
12. Redis server log

**Sources** — new places to read bytes from, in `internal/source/`:

13. Read a `.tar.gz` archive without extracting it first
14. zstd decompression. `walk.go` currently lists `.zst` among the extensions it
    skips; this is that line plus a decompressor and a fixture

**Query**:

15. `--extract 'took (?<ms>\d+)ms'` — pull named fields out of unstructured
    messages at query time, so the fallback text parser stops being a dead end.
    Larger than the others and worth discussing on the issue first
