# Contributing

Thanks for looking. The most valuable contribution to this project is **a parser for a log
format you actually have** — that is the whole reason this project can be useful to people
whose logs look nothing like mine.

A parser is one file, roughly 100 lines, plus a fixture. The walkthrough below should take
about twenty minutes.

---

## Setup

```bash
git clone https://github.com/VIGIL-OPS/loupe && cd loupe
go build ./cmd/loupe          # requires Go 1.22+, CGO enabled
go test ./...
./loupe testdata/jsonl/sample.log
```

CGO is required because we link DuckDB. On Linux you need a C toolchain
(`build-essential`); on macOS, the Xcode command line tools.

For frontend work: `cd web && npm install && npm run dev`, which proxies the API to a
`loupe serve` running on the default port.

---

## Adding a parser — the walkthrough

Say you want to support Nginx error logs.

### 1. Add a fixture

Create `testdata/nginx-error/sample.log` with 20–50 real-looking lines. **Make it messy on
purpose:** include a blank line, a truncated final line, and at least one malformed record.
Scrub any real hostnames, IPs, or tokens.

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
    ts, err := time.Parse("2006/01/02 15:04:05", string(m[1]))
    if err != nil {
        // A missing or unparseable timestamp is NOT a fatal error.
        // Return the record with a zero Timestamp; it stays queryable.
        ts = time.Time{}
    }
    return Record{
        Timestamp: ts,
        Level:     NormaliseLevel(string(m[2])),
        Message:   string(m[3]),
        Fields: map[string]any{
            "client":  string(m[4]),
            "request": string(m[5]),
        },
    }, nil
}
```

Three rules that cover most review comments:

- **Never return a fatal error for one bad line.** Return `ErrNoMatch` and the pipeline will
  fall back gracefully. One corrupt line must not abort a 4GB file.
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

## Other good contributions

- **Sources** — new places to read bytes from (`internal/source/`). Same shape: one file, one
  interface, one fixture.
- **Timestamp formats** — `internal/schema/timestamps.go` holds the layouts tried during
  inference. Adding one is a two-line PR with a test, and genuinely useful.
- **Performance** — bring a benchmark. `go test -bench` before and after, both numbers in the
  PR description.
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

- `gofmt`, `go vet`, and `golangci-lint run` must pass. CI enforces this.
- Conventional commits (`feat:`, `fix:`, `docs:`, ...), imperative mood, under 72 characters.
- New behaviour needs a test.
- Discuss new third-party dependencies in an issue first.
- Be decent to each other in issues and reviews.

---

## Seed issues to label `good first issue`

*(Maintainer note — create these before announcing anywhere. Each should link to
`logfmt.go` as the template and include a sample of the target format.)*

1. Parser: Nginx error log
2. Parser: Nginx / Apache access log (combined format)
3. Parser: Python `logging` default format
4. Parser: Java Log4j / Logback default pattern
5. Parser: Rails production log
6. Parser: Kubernetes CRI log format
7. Parser: AWS CloudTrail JSON
8. Parser: Caddy JSON access logs
9. Parser: PostgreSQL server log
10. Parser: systemd `journalctl -o json`
11. Source: read from a `.tar.gz` archive without extracting
12. Source: zstd decompression
13. Timestamp layouts: add epoch-microseconds detection
14. Render: add CSV output format
15. UI: keyboard shortcut to focus the filter box
16. Detect rotated-log ordering for `app-2026-01-01.log` style names
17. Improve the truncated-final-line case in the directory walker
