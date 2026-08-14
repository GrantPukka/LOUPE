# Architecture

**Working name:** `loupe` — a jeweller's magnifier. Short, typeable, memorable. Check
crates/npm/GitHub for collisions before you commit to it; `logpond`, `sift`, and `tailcall`
are decent fallbacks.

**One-line pitch:** Point it at a directory of logs, get a searchable UI in one second, with
no Elasticsearch, no Docker Compose, no daemon.

---

## 1. Design constraints

These are the decisions that everything else follows from. Violating one of these should
require a deliberate discussion, not a drive-by PR.

| Constraint | Why |
|---|---|
| **Single static binary, no runtime dependencies** | The moment adoption requires `npm install` or a running service, you lose 80% of drive-by users. `brew install loupe` and you're done. |
| **CLI is the product; UI is a view over it** | Everything the UI does must be doable from the CLI first. Keeps the core testable and keeps scope creep out of the frontend. |
| **Read-only, local-first** | No ingestion daemon, no write path, no auth, no multi-user. This is the thing that makes the project finishable. |
| **Zero-config default path** | `loupe ./logs` must work on a messy real-world directory with no flags. Every flag you add is a small admission of failure. |
| **Parsers are the contribution surface** | Adding support for a new log format must be a ~100 line, single-file, copy-the-template job. This is how the project gets contributors. |

---

## 2. Technology choices

**Go.** Single-binary distribution is the whole ballgame here, and the observability
community reads Go natively — which matters for drive-by PRs.

**DuckDB via `marcboeker/go-duckdb`** for the query engine. You get columnar storage, a
mature SQL dialect, JSON functions, and Parquet reading for free. Writing your own query
engine would eat the entire project.

> **Known cost, decide with eyes open:** go-duckdb requires CGO. It bundles prebuilt static
> libraries for darwin/linux on amd64 and arm64, so those platforms are fine, but you cannot
> cross-compile from a single runner — you need a CI matrix on native runners. Windows is the
> weak spot; plan to ship it late or via WSL guidance. Binary size lands somewhere around
> 40–60MB. Both are acceptable prices for not writing a query engine. Document them in the
> README so nobody is surprised.
>
> **Escape hatch** if CGO becomes intolerable: the `store` package interface below is narrow
> enough that a pure-Go Arrow-based backend could be swapped in behind it. Do not build this
> speculatively.
>
> **Measured on linux/amd64, Go 1.24.0, go-duckdb v1.8.5** — a throwaway program creating an
> in-memory table, appending 100k rows, and reading them back:
>
> | | |
> |---|---|
> | Cold build, including the CGO link | 40s |
> | Binary size | 46MB |
> | Appender ingest | 100k rows in 140ms (713k rows/sec) |
> | `GROUP BY` over 100k rows | 5ms |
>
> Binary size landed inside the predicted 40–60MB and the appender is roughly an order of
> magnitude clear of what the 1GB-in-20s ingest target needs. go-duckdb requires **Go 1.24 or
> newer**, which sets the project's floor.

**Frontend: Preact + Vite**, built to static assets and embedded with `//go:embed`. Preact
over React purely for bundle size — the whole UI should be under 100KB. No component library,
no CSS framework beyond a small hand-written stylesheet. One screen does not need a design
system.

---

## 3. Pipeline

```
  Source ──▶ Parser ──▶ Schema inference ──▶ Store (DuckDB) ──▶ Query ──▶ Render
 (bytes)   (records)      (columns)            (tables)         (SQL)   (CLI | HTTP)
```

Each stage is a package with a narrow interface. Data flows one way. No stage imports a
stage downstream of it.

### 3.1 Source — *where bytes come from*

```go
type Source interface {
    Name() string                       // display name, usually a path
    Open(ctx context.Context) (io.ReadCloser, error)
    Size() int64                        // -1 if unknown (streams)
    Fingerprint() string                // path+size+mtime, for cache invalidation
}
```

v1 implementations: local file, local directory walk (with glob + ignore rules), stdin,
transparent gzip/zstd decompression. Later: S3/GCS prefix, `journalctl`, Docker `json-file`.

Directory walking should skip binaries and anything over a configurable size ceiling, and
should recognise rotated-log naming (`app.log`, `app.log.1`, `app.log.2.gz`) so it can order
them chronologically.

### 3.2 Parser — *bytes to records*

**This is the extension point. Guard its simplicity aggressively.**

```go
type Record struct {
    Timestamp time.Time      // zero value if none found
    Level     string         // normalised: trace/debug/info/warn/error/fatal
    Message   string
    Fields    map[string]any // everything else
}

type Parser interface {
    Name() string
    // Detect returns 0.0–1.0 confidence given a sample of lines from the head of a source.
    Detect(sample [][]byte) float64
    // Parse converts one logical line into a Record.
    Parse(line []byte) (Record, error)
}
```

Parsers register themselves in an `init()` into a package-level registry. Adding a format
means: one new file, one `init()`, one golden-file fixture. Nothing else in the codebase
changes. That property is worth defending in code review.

v1 formats: JSON lines, logfmt, Apache common + combined, syslog RFC5424, Go `slog` text,
and a permissive fallback that treats the whole line as `Message` and best-effort extracts a
leading timestamp.

Detection runs all parsers against the first ~200 lines and takes the highest confidence.
`--parser=name` overrides. Multi-line records (Java stack traces, Python tracebacks) are
handled by a continuation rule at the source level — a line that fails to parse and doesn't
start with a timestamp gets appended to the previous record's `Message`.

### 3.3 Schema inference — *records to columns*

Sample the first N records (default 10,000). Any field appearing in more than 60% of them
gets promoted to a real typed DuckDB column with an inferred type. Everything else stays in
a `fields` JSON column, still queryable via DuckDB's JSON operators, just slower.

This is the piece that makes the tool feel smart rather than generic, and it is where you
should spend your careful thinking. Type inference needs to handle the classic traps:
numeric-looking strings that are actually IDs, ISO-8601 vs epoch millis vs epoch seconds,
and fields whose type changes halfway through a file.

**v1 simplification:** ship with fixed promotion (`ts`, `level`, `message`, `source`,
`line_no`, `raw`, `fields`) and add dynamic promotion in the second pass. Do not let schema
inference block the first working demo.

**As built (second pass).** Two deliberate departures from the paragraph above, both
forced by measurement:

- **Coverage is judged per source, not across the directory.** The 60% figure assumes one
  coherent log stream. Measured on the demo directory, the single most common field of all
  reaches 59.9%, because six formats each contribute their own vocabulary — so a global
  threshold promotes nothing at all. A field carried by every Nginx record is a good column
  even though Postgres never sets it; that is what NULL is for. Both the global and the
  per-source figures are recorded on each `Promotion` so the decision can be explained.
- **The sample is stratified and deterministic.** An equal slice from the head of each
  source, not the head of the table: the first 10,000 records of the demo directory are all
  Nginx, so a naive head sample promotes Nginx's columns and nothing else. It is not random,
  because the decision is cached and a schema that wobbled between runs over identical files
  would be indefensible.

Promotion rebuilds the table once with a single `CREATE TABLE AS`, rather than an `ALTER`
plus `UPDATE` per column, which would rewrite the whole table once per promoted field.
`TRY_CAST` means one unparseable value yields NULL for that row rather than failing the
rebuild. Promoted keys stay in the `fields` bag as well: stripping them needs a per-row JSON
rewrite, and `raw` already holds a copy of everything.

### 3.4 Store — *DuckDB wrapper*

```sql
CREATE TABLE logs (
    ts       TIMESTAMP,
    level    VARCHAR,
    message  VARCHAR,
    source   VARCHAR,   -- originating file
    line_no  BIGINT,
    raw      VARCHAR,    -- original line, always kept
    fields   JSON        -- unpromoted fields
);
```

Ingest via DuckDB's Appender API, not row-by-row `INSERT` — the difference is roughly an
order of magnitude.

**Caching:** ingested data persists to `~/.cache/loupe/<fingerprint>.duckdb`, where the
fingerprint hashes source paths, sizes, mtimes, and the parser version. A re-run over
unchanged files skips ingestion entirely. This is what makes the tool feel instant on the
second invocation, and it is a bigger perceived-quality win than any amount of query
optimisation. `--no-cache` bypasses it.

**As built.** Measured on the demo directory: **1.15s cold, 0.14s warm.**

- `--source-tz` is in the fingerprint. It moves timestamps, so a key without it would serve
  records hours out.
- `IngestVersion` is in the fingerprint. See CONTRIBUTING.md — forgetting to bump it means
  users keep reading data produced by superseded parsers.
- Ingest writes to `<fingerprint>.duckdb.partial` and renames on success, so an interrupted
  run never leaves a half-built database a later run would trust.
- The cached file carries a summary of the load, so a cache hit still reports the same
  unparsed counts and assumed timezones a cold run did. A tool that gets quieter about its
  own caveats on the second run is worse than one that never mentioned them.
- The directory is capped at 2GiB with least-recently-used eviction, never evicting the
  entry the current run just wrote.

**Known limitation:** an actively written log file changes size and mtime on every run, so a
live directory re-ingests each time. The cache pays off on archived and rotated logs. Making
this incremental means storing per-file byte offsets and appending only the tail, which
interacts with `--follow` and is not yet built.

### 3.5 Query — *two front doors*

**Raw SQL** (`loupe sql "SELECT ..."`) for power users. Passed through to DuckDB unchanged.

**Filter DSL** for everyone else, compiled to SQL:

```
level:error                      → level = 'error'
level:error,warn                 → level IN ('error','warn')
status:>=500                     → CAST(fields->>'$.status' AS BIGINT) >= 500
message~"timeout"                → message ILIKE '%timeout%'
user_id:abc123                   → fields->>'$.user_id' = 'abc123'
-level:debug                     → NOT (level = 'debug')
last:15m / since:2026-01-01      → ts >= ...
```

Build it as a proper lexer + parser producing an AST, then compile the AST to parameterised
SQL. It is tempting to do this with string concatenation and regex; that path ends in
injection bugs and unfixable precedence problems around two weeks in.

### 3.6 Render

- `table` — aligned, colourised, truncated to terminal width (default for TTY)
- `json` / `ndjson` — for piping (auto-selected when stdout is not a TTY)
- `raw` — original lines, for `grep` compatibility
- HTTP JSON — for the web UI

---

## 4. Module layout

```
cmd/loupe/              main + cobra command wiring only, no logic
internal/
  source/               Source interface, local, stdin, compress, walk
  parse/                Parser interface, registry, detect
    jsonl.go  logfmt.go  apache.go  syslog.go  slogtext.go  fallback.go
  schema/               inference, type coercion, field promotion
  store/                DuckDB lifecycle, appender ingest, cache management
  query/                filter DSL lexer/parser/AST, SQL compiler
  server/               HTTP handlers, //go:embed of web/dist
  render/               table, json, raw writers
  tui/                  full-screen terminal interface behind `loupe tui`
web/                    Preact + Vite source; `npm run build` → web/dist
testdata/               golden fixtures, one directory per format
docs/                   ARCHITECTURE.md, adding-a-parser.md
```

**Dependency rule:** `parse` must not import `store`, `query`, or `server`. `store` must not
import `server`. If a parser needs to know about SQL, the design has gone wrong.

### 4.1 Third-party dependencies, and why each one is here

The allowed set is small on purpose. Additions need a reason recorded in this table.

| Dependency | Why it earns its place |
|---|---|
| `spf13/cobra` | Subcommands, flags, and help output. Not worth hand-rolling. |
| `marcboeker/go-duckdb` | The query engine. Writing one would eat the whole project. |
| `charmbracelet/bubbletea` + `lipgloss` | The TUI in §6.1. Bubble Tea is the Elm-architecture terminal framework the Go ecosystem has settled on, and lipgloss fills the colour-library slot the allowed set already reserved. |

Explicitly declined, and expected to be proposed again: a logging framework, a DI container,
a config library, an ORM, and an assertion library for tests. Prefer the standard library.

---

## 5. HTTP API

Tiny, and the UI must not need anything beyond it.

```
GET  /api/schema                      → promoted columns, types, distinct-value samples
POST /api/query   {filter|sql, limit, offset, sort}
                                      → {columns, rows, total, took_ms}
POST /api/histogram {filter, bucket}  → [{bucket_start, count, level_breakdown}]
GET  /api/tail    (SSE)               → live records, for `loupe serve --follow`
```

## 6. The UI — one screen, and it stays one screen

```
┌──────────────────────────────────────────────────────────┐
│ [ filter box                                    ] [Run]  │
├──────────────────────────────────────────────────────────┤
│ ▁▂▅█▅▂▁▁▂▃▅▂▁  timeline, click-drag to zoom to a range   │
├──────────────────────────────────────────────────────────┤
│ ts        level  message                    ▸ expand row │
│ …virtualised, infinite scroll…                           │
└──────────────────────────────────────────────────────────┘
```

Expanding a row shows the full record and its raw line. Clicking a field value inserts it
into the filter box. That single interaction is most of the perceived magic — build it early.

**Explicit non-goals for the UI:** saved searches, dashboards, user accounts, alerting,
multiple tabs, a settings page, dark/light toggle (pick one, respect `prefers-color-scheme`,
move on). Every one of these is how a weekend project becomes an abandoned one.

### 6.1 The TUI

`loupe tui` is the same screen in the terminal, for the common case of being SSH'd into a box
with no browser and no way to copy a 4GB log directory off it. That case is frequent enough
in the target user's life to be worth one dependency.

It is a third *view*, not a third *implementation*. It calls the same query path as the CLI
and the HTTP API. The same rule the web UI lives under applies: if the TUI can do something
the CLI cannot, that is a bug in the CLI.

Its non-goals are the web UI's non-goals, plus mouse-driven chrome. Keyboard first.

---

## 7. Milestones

**M1 — the spine (week 1).** stdin + single file + directory walk; JSON lines and logfmt
parsers; DuckDB ingest via appender; `loupe sql`; table and json renderers. Success test: it
answers a real question about a real log file faster than `jq` does.

**M2 — the ergonomics (week 2).** Filter DSL; timestamp inference; cache layer; remaining v1
parsers; format auto-detection; CLI timeline histogram. Success test: you reach for it
instead of `grep` on your own machine, unprompted.

**M3 — the UI (week 3).** HTTP API; Preact single screen; virtualised table; click-to-filter;
embedded assets; `loupe serve`. Success test: the ten-second demo GIF is recordable.

**M4 — the launch (week 4).** Cross-platform CI matrix and release binaries; Homebrew tap;
README with the GIF; `docs/adding-a-parser.md`; **15 labelled `good first issue` tickets
merged before you post anywhere.**

That last item is not optional. The most common failure mode for a repo that gets attention
is having nothing for interested people to do on the day they arrive.

---

## 8. Non-goals, stated so they can be closed as "won't fix"

- Distributed or clustered operation
- Persistent ingestion daemon / log shipping
- Alerting, monitors, notifications
- Authentication, multi-tenancy, RBAC
- Metrics or traces (logs only — resist "why not OTel traces too?")
- Being a Datadog/Splunk replacement

The pitch is "I have logs on disk right now and I want to look at them." Everything that
isn't that is someone else's project.
