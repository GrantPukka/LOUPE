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
transparent gzip/zstd/bzip2/xz decompression. Later: S3/GCS prefix.

Compression is recognised from magic bytes, never from the extension: rotated logs are named
inconsistently, and a `.log` that is really gzip is common. A compressed source is never
`Tailable` — an offset into decompressed bytes cannot be seeked to — which is what EC001's
incremental path already assumes.

A symlink to a regular file is followed; a symlink to a directory is not, because it can
point at its own ancestor. Files reachable under two names are read once — deduplicated by
resolved path, since `Fingerprint` is built from the path and would not catch it — and the
duplicates are counted rather than dropped quietly.

Directory walking should skip binaries and anything over a configurable size ceiling, and
should recognise rotated-log naming (`app.log`, `app.log.1`, `app.log.2.gz`) so it can order
them chronologically. The archive suffix is stripped by one shared function, so a rotation
group and a logical source name cannot disagree about which files belong together.

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

**Incremental re-ingest.** A growing file is appended to rather than re-read. The
fingerprint covers the *identity* of the source set — paths, parser, zones, ingest version —
and deliberately not file sizes or mtimes, so a directory being written to keeps mapping to
the same cache entry. Per-file state lives in `loupe_cache_files`:

- **head hash** — the first 4KiB. This, not size, is what distinguishes appending from
  rewriting: a file truncated and refilled to the same length is otherwise indistinguishable,
  and would serve records that no longer exist.
- **resume offset and line** — the start of the *last record read*, not the end of the file.
  A file read while it is being written can stop mid-record, and a stack trace is one record.
  Resuming past it would leave its remaining frames as orphan records. The last record is
  therefore re-read, and rows at or after its line number are discarded first, which makes
  the append idempotent.
- **`before` stats** — the file's totals excluding that last record, so adding the resumed
  read's stats totals exactly. The status line's counts are claims about the data; an
  off-by-one from double-counting the boundary record is a wrong claim.

A file that has become unreadable is left alone rather than dropped: re-reading would fail
anyway, and discarding records we hold on no evidence they are wrong is the silent loss this
project refuses. A file that has disappeared from the walk does have its rows discarded — the
cache mirrors the current source set.

**Cost note.** Ingest appends the base columns, so the table must shed its promoted columns
before an append and re-run inference after. That rebuild, not the read, dominates a refresh:
appending 500 lines to a 5MB directory takes 0.93s against 1.72s for a full re-read. The read
itself is proportional to what was appended; the promotion rebuild is proportional to the
whole table.

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
stats count() by level           → SELECT level, count(*) … GROUP BY 1
stats p99(latency_ms) by bin(1m) → time_bucket(…) with the aggregate over it
```

Build it as a proper lexer + parser producing an AST, then compile the AST to parameterised
SQL. It is tempting to do this with string concatenation and regex; that path ends in
injection bugs and unfixable precedence problems around two weeks in.

A filter says which records; an optional `stats` clause says what to report about them, so
the same string can produce a listing or a summary. `Session.Plan` refuses a clause that
reaches something which lists records rather than dropping it — see docs/FILTER-DSL.md
section 10.

`loupe diff` resolves two windows through that same path and compares them. It ranks by a
log-likelihood ratio over each item's share of its window rather than by raw delta, which
is what keeps a change in traffic volume from filling the list with itself; rates are what
is *displayed*, because windows of unequal length are a legitimate comparison.

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
  session/              the query path shared by every front end
  server/               HTTP handlers, //go:embed of web/dist
  render/               table, json, raw writers
  tui/                  full-screen terminal interface behind `loupe tui`
web/                    Preact + Vite source; `npm run build` → web/dist
testdata/               golden fixtures, one directory per format
docs/                   ARCHITECTURE.md, adding-a-parser.md
```

**Dependency rule:** `parse` must not import `store`, `query`, or `server`. `store` must not
import `server`. If a parser needs to know about SQL, the design has gone wrong.

`workspace/` holds the remembered log locations and the append-only audit trail. It is a
list of paths and a history, not an ingestion daemon — the non-goals in §8 still hold, and
loupe still only reads when you run it.

`session` sits above `store` and `query` and below every front end. It exists because the
CLI, the HTTP API, and the TUI must share one query path; anything that would otherwise be
duplicated into a second front end belongs there rather than in `cmd/loupe`.

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

**As built.** Nine endpoints: `/api/schema`, `/api/query`, `/api/histogram`, `/api/sources`,
`/api/tail`, `/api/patterns`, `/api/trace`, `/api/trace-field`, and `/api/top`, plus
`/api/health`. All of them call `internal/session`, which is the same
code path `cmd/loupe` uses — a capability reachable over HTTP but not from the terminal
would be a bug.

- **Loopback only, enforced.** `Listen` refuses a non-loopback address rather than
  documenting the risk. There is no authentication, so binding to a reachable interface
  would publish the logs; the error suggests an SSH tunnel. No CORS headers are sent.
- **The Host header is checked on every request.** Binding to loopback was enough while the
  API only served a directory named on the command line. It stopped being enough when
  `/api/browse` was added: a page on another origin can point a hostname it controls at
  127.0.0.1, wait for the DNS cache to flip, and reach a loopback service as same-origin.
  The browser sends the attacker's hostname in `Host`, which is what the check catches.
  Browsing is separately confined to a fixed set of roots, so the two defences are
  independent.
- **Errors arrive verbatim.** A typo'd field name returns the same spelling suggestion and
  field list the CLI prints. A UI that shows "bad request" where the terminal shows a fix is
  worse than the terminal at the moment the user most needs help.
- **Every disclosure travels with the response**: the resolved window in both zones, the
  resolution notes, the count a time filter excluded for having no timestamp, and the
  explanation for an empty result. The API must not be quieter about its caveats than the
  CLI is.
- **Timestamps are UTC instants** and the display timezone is named separately, so a client
  formats them itself rather than guessing whose clock it is reading.

`/api/tail` streams live records as server-sent events. `loupe ./logs --follow` implemented
following in the CLI first, which is what CLAUDE.md requires before it appears over HTTP, on
top of the incremental reads described in §3.4.

- **One follower per server, not per connection.** A `Follower` carries its own record of
  where it has read to in each file, and a poll rewinds to the start of the last record to
  re-read it. Two of them over the same store would each rewind past the other's writes:
  two open tabs would see duplicated lines in one and missing lines in the other. Sharing
  one also keeps the store's writes on a single goroutine, which is what makes them safe
  alongside the query handlers.
- **The poll loop runs only while somebody is subscribed**, and stops when the last client
  disconnects. An idle `loupe serve` polls nothing and opens no files. There is still no
  daemon.
- **Each connection carries its own filter**, and its rows are selected by ANDing its
  compiled plan with `Batch.Predicate()` — the same predicate the CLI uses, so the stream
  and a query run afterwards cannot disagree.
- **The per-subscriber queries run on the poll goroutine**, before it returns. The predicate
  excludes a boundary record by file and line number, and the next poll may delete and
  reinsert that record under a new sequence number, so a query deferred past the next poll
  would select the wrong rows.
- **A slow client is told it fell behind.** Its buffer is bounded, and overflowing it sends a
  `lag` event naming the number of dropped updates and saying the records are still in the
  store. Blocking would stall every other client on one unresponsive tab; dropping silently
  would be a live tail that quietly stopped being complete.
- **Read failures travel as `notice` events.** A file that becomes unreadable mid-incident
  stops that source, not the stream, and says which one.
- **The write deadline is cleared for this route only.** The server sets one so a slow query
  cannot hold a connection forever; a live tail is the one response meant to stay open.

The UI's live view is opt-in. Opening the page must not start polling somebody's log
directory, and a reader who has narrowed to a window last Tuesday should not be dragged to
the present. While it is on, arrivals enter the existing table and the timeline is redrawn on
a throttle — and if the reader has scrolled away from the top, records are held and offered
as a count rather than inserted, because shuffling rows under somebody mid-incident is how
they lose the line they were looking at.

`loupe tui --follow` is the same behaviour in the terminal. That list is oldest-first, so
arrivals append and the cursor rides the end only if it was already there; otherwise the
footer counts what has landed below and `G` goes to it.

Following polls with `os.Stat` on a 400ms ticker rather than using filesystem notifications.
No new dependency, identical behaviour on every platform, and it keeps working on NFS and
bind mounts where inotify silently misses events — a log tool that quietly stops showing new
lines is worse than one that never offered to. There is no goroutine and no schedule inside
the store: `Follower.Poll` does one pass and returns, so following happens only while someone
is watching, which is what keeps the no-daemon promise literal.

New records cannot be appended to `logs` directly, because schema inference has widened it
beyond the shape the appender writes. They are staged in a side table and inserted with the
promoted columns computed from the fields bag, so the cost is proportional to what arrived
rather than to the size of the dataset. Getting this wrong is not a performance bug: live
records would arrive with promoted columns NULL, and `status:>=500` would silently fail to
match the incident being watched.

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

The pattern rail is the one addition to the single screen, and it is off until asked for.
It costs a grouping query and takes width from the message column, so making it permanent
would be a change to the screen rather than an option on it. Clicking a template writes a
real `pattern:<id>` term into the filter box — the same principle as the timeline drag, so
the interaction teaches the syntax and stays shareable.

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

**As built.** Also a Bubble Tea TUI (`loupe tui`) and the handoff export, which the milestone
list put in M4. Both are views over `internal/session`, so all three front ends — terminal,
browser, and extract — run the same query path. Following is built for all three: `--follow`
in the CLI and the TUI, and `/api/tail` for the browser. See §5.

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
