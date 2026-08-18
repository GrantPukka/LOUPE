# Roadmap

Work items, each with an `EC` number used as its branch name and referenced in
commits. Tiers are by value, not by order — within a tier, pick by what unblocks
what.

Every item here stays inside the invariants in `CLAUDE.md`: single static
binary, CLI before UI, read-only, no network, no daemon. An item that cannot be
built without breaking one of those is not on this list.

**Status vocabulary:** `not started` · `in progress` · `done` · `blocked`

| EC | Item | Tier | Status |
|---|---|---|---|
| [EC001](#ec001--live-tail---follow--incremental-ingest) | Live tail + incremental ingest | 1 | **done** — EC001.4 optional, not started |
| [EC002](#ec002--pattern-clustering--message-grouping) | Pattern clustering / message grouping | 1 | not started |
| [EC003](#ec003--first-class-tracerequest-correlation) | Trace / request correlation | 1 | not started |
| [EC004](#ec004--wire-up-stdin-streaming) | Wire up stdin streaming | 1 | not started |
| [EC005](#ec005--faceted-breakdowns--top-n) | Faceted breakdowns / top-N | 2 | not started |
| [EC006](#ec006--aggregations-in-the-dsl) | Aggregations in the DSL | 2 | not started |
| [EC007](#ec007--window-compare--diff) | Window compare / diff | 2 | not started |
| [EC008](#ec008--broaden-intake) | Broaden intake | 2 | not started |
| [EC009](#ec009--ad-hoc-regex-field-extraction) | Ad-hoc regex field extraction | 3 | not started |
| [EC010](#ec010--self-contained-html-report-export) | Self-contained HTML report export | 3 | not started |
| [EC011](#ec011--query-history-recall) | Query history recall | 3 | not started |

---

# Tier 1 — multiplies the core value

## EC001 — Live tail (`--follow`) + incremental ingest

**Status: done.** Stages 1, 2 and 3 complete and tested. EC001.4 remains as an
optional follow-up and is deliberately not started. Work is on branch `EC001`.

Turns loupe from "look at yesterday's logs" into "watch this incident unfold".
The two halves are one project: following a file is incremental reading with a
timer on it.

### EC001.1 — Incremental ingest — **done**

- [x] `parse.ReadAll` reports a `Tail` — byte offset, line number, and the stats
      as they stood before the last record
- [x] `source.Tailable` optional interface (`OpenAt`, `Head`), kept off `Source`
      so a stream is not forced to implement it
- [x] `Fingerprint` covers source *identity* only; size and mtime moved into a
      per-file `loupe_cache_files` table
- [x] Plan per file on re-open: skip / append / re-read, decided on a head hash
      rather than size, so a truncate-and-refill cannot pass as unchanged
- [x] Resume at the *start* of the last record, discarding rows at or after its
      line number — a stack trace half-written at EOF must not be orphaned
- [x] `IngestVersion` 4 → 5
- [x] Tests: resume reproduces byte-identical records and line numbers; stats
      total exactly across a resume; rewritten file re-read; summary matches the
      table after an append
- [x] `ARCHITECTURE.md` §3.4 rewritten — it documented this as a known limitation

Two bugs found by verification, both silent: appends failed with `invalid column
count` because promotion had widened `logs` beyond the appender's shape, and
`writeCacheMeta` inserted a second metadata row so the run *after* an append
reported pre-append counts.

Honest cost: 0.93s incremental against 1.72s full re-read on 5MB. The read is
proportional to what was appended, but the demote/re-promote rebuild is
proportional to the whole table and dominates. Fixing that means promoted
columns becoming a view over a base table — a real architectural change, tracked
as EC001.4 below and deliberately not attempted yet.

### EC001.2 — `loupe ./logs --follow` — **done**

- [x] `store.Follower` polling `os.Stat` on a 400ms ticker — no new dependency,
      works on NFS where inotify silently misses events
- [x] No goroutine or schedule inside the store: `Poll` does one pass and
      returns, so following happens only while someone is watching
- [x] New records staged in a side table and inserted with promoted columns
      computed, so cost is proportional to what arrived, not to the dataset
- [x] `Batch.Predicate()` excludes the re-read boundary record, so a completed
      record is never printed twice
- [x] Sources re-resolved on every poll, so a file created mid-incident appears
- [x] `last:` anchors to the wall clock in follow mode per `docs/FILTER-DSL.md`
      §2.2; an explicit `--relative-to` still wins
- [x] Live records run through the same compiled DSL as everything else
- [x] Tests: quiet poll adds nothing, appended records appear exactly once,
      promoted columns populated on live records, new file picked up, record
      split across ticks completed
- [x] `README.md` and `ARCHITECTURE.md` §5 updated

Bug found by testing: the head hash covered a fixed 4KiB window, so for any file
smaller than that every append changed the hashed bytes and looked like a
rewrite — follow mode would have re-read whole files forever.

Accepted and documented: a record that gains continuation lines after it was
first emitted is corrected in the store but not reprinted. Printing the same
line twice in a live tail is worse.

### EC001.3 — `/api/tail` and the live UI — **done**

- [x] `GET /api/tail` as SSE — as specified at `ARCHITECTURE.md` §5
- [x] One server-side follower **shared by every connection**, with the poll
      loop starting on the first subscriber and stopping when the last leaves
- [x] Reuse `Batch.Predicate()` so the stream and a later query agree
- [x] Browser `EventSource` client; new rows enter the existing table, and the
      histogram is redrawn on a throttle, without a reload
- [x] Pause-on-scroll: records are held and offered as a count, and clicking
      the count shows them and jumps to the top
- [x] TUI streaming (`loupe tui --follow`)
- [x] Playwright coverage of the live path — four specs against the real binary
- [x] `ARCHITECTURE.md` §5, `README.md`, and `loupe serve --help` updated

**Corrected from the plan above: one follower per server, not per connection.**
A `Follower` carries its own record of where it has read to in each file, and a
poll rewinds to the start of the last record to re-read it. Two of them over
one store would each rewind past the other's writes, so two open tabs would see
duplicated lines in one and missing lines in the other. Sharing one also keeps
the store's writes on a single goroutine, which is what makes them safe
alongside the query handlers.

The per-subscriber queries run on the poll goroutine before it returns, rather
than the batch being handed out to be queried later. `Predicate()` excludes the
boundary record by file and line number, and the next poll may delete and
reinsert that record under a new sequence number — a query deferred past the
next poll would select the wrong rows.

Bug found and fixed in stage 1's code: `writeFileStates` ran only when a cache
file was being written, so `--no-cache` left a follower with no offsets to
resume from. Its first poll planned a re-read of every file and republished the
whole dataset as if it had just arrived. Invisible in the CLI, where the first
poll's output looks like a busy log; fatal in the UI, where it floods the
table. The offsets are now recorded either way — they describe where a read got
to, which follow mode needs whether or not the database is being kept.

Deliberate deviations, both under invariant 5: there is **no `serve --follow`
flag** — the endpoint is always present and idle until subscribed — and the
UI's live view is **off until switched on**, so opening the page never starts
polling somebody's log directory. Following pins the sort to newest-first while
it is on, because arrivals appended to the bottom of an oldest-first list land
off the end of a page nobody is looking at.

A slow client has a bounded buffer; overflowing it sends a `lag` event naming
the number of dropped updates and saying the records are still in the store.
Blocking would stall every other client on one unresponsive tab, and dropping
silently would be a live tail that had quietly stopped being complete.

### EC001.4 — Remove the promotion rebuild from the refresh path — **not started**

Optional follow-up, only if refresh latency becomes a complaint.

- [ ] Ingest into a base table, expose `logs` as a view with promoted columns
- [ ] Removes demote/re-promote from re-open entirely
- [ ] Benchmark before and after; do not start without a number that justifies it

---

## EC002 — Pattern clustering / message grouping

**Status: not started.** Tier 1. The highest-leverage triage feature: *"34,000
lines → 12 distinct templates, and this one is new in the last 5 minutes."*

Collapses `user 4821 timed out` and `user 9903 timed out` into one pattern with a
count, so the anomaly is visible in a wall of noise. Pure read-only computation
over data already in DuckDB, and precisely what grep cannot do.

- [ ] Drain-style templating: tokenise a message, replace variable positions with
      wildcards, group by the resulting template
- [ ] Decide where it runs — SQL in DuckDB, or Go over a scan. Benchmark both;
      1GB in 20s ingest is the yardstick for what "fast enough" means here
- [ ] `loupe patterns ./logs [filter]` — template, count, first seen, last seen,
      example line, sources it appears in
- [ ] `--new-since <window>` to surface templates absent from an earlier window
      (this is the feature, the rest is table stakes)
- [ ] A template must expand back to its matching records: `pattern:<id>` as a
      DSL term, or the view is a dead end
- [ ] Stable template ids across runs, or `--new-since` compares nothing
- [ ] UI: pattern list as a left rail, click to filter
- [ ] Golden tests over the demo directory, which has six formats and real noise
- [ ] Numeric ids, UUIDs, IPs, paths, and quoted strings must all be recognised
      as variable, or every line becomes its own template

**Watch:** the failure mode is over-collapsing — merging two genuinely different
errors into one template hides the incident. Prefer too many templates to too
few, and make the collapse rule inspectable.

---

## EC003 — First-class trace / request correlation

**Status: not started.** Tier 1. The demo already brags that one `trace_id` runs
through all six sources; that is currently the pitch, not a feature.

Delivers the README's exact promise: watch the pool exhaust, then the app error,
then Nginx 502, in the order it happened.

- [ ] `loupe trace <id> ./logs` — one request's timeline across every source
- [ ] Show the latency gap between consecutive hops; the gap is the finding
- [ ] Auto-detect the correlation field: `trace_id`, `traceId`, `request_id`,
      `req_id`, `x-request-id`, `correlation_id` — configurable, detected by
      default, because a flag here is an admission of failure per invariant 5
- [ ] Handle a trace present in some sources and absent from others without
      implying the request skipped a service
- [ ] Records with no timestamp still belong to the trace — order them last and
      say so, never drop them
- [ ] UI: click any trace value to open the trace view
- [ ] Handoff export of a single trace, which is what gets pasted into an incident
      channel
- [ ] Tests over the demo directory, whose shared trace id is the fixture

---

## EC004 — Wire up stdin streaming

**Status: not started.** Tier 1, and the cheapest item on this list.

`internal/source/file.go:113` defines `Stdin` and its comment says it is for
``kubectl logs -f api | loupe``. Verified: **`NewStdin` has no callers outside
tests.** The type is complete and unreachable, so the documented pipeline does
not work today.

Opens the whole container ecosystem with no new concepts and no daemon:
`kubectl logs -f`, `docker logs`, `journalctl -f` all pipe straight in.

- [ ] Detect a piped stdin and use it when no path is given
- [ ] Explicit `-` as a path argument, which composes with real paths
- [ ] Never block on an interactive terminal waiting for input nobody is typing
- [ ] Uncacheable by design — `Fingerprint()` already returns empty, so confirm
      `OpenCached` degrades cleanly and *says why* in the status line
- [ ] A stream has no size, so progress reporting must not divide by it
- [ ] Streaming mode: render as records arrive rather than after EOF, or
      `kubectl logs -f` hangs silently forever
- [ ] Interaction with EC001: a stream is not `Tailable` and must never be asked
      to seek
- [ ] Piped gzip already works via magic-byte detection — keep a test on it
- [ ] Tests: piped fixture, empty stdin, stdin plus a directory, stdin closed
      mid-record

---

# Tier 2 — high value, lower effort

## EC005 — Faceted breakdowns / top-N

**Status: not started.** Answers the most common triage question — *"which
endpoints are 500ing?"* — without dropping to SQL. A `GROUP BY` DuckDB does for
free.

- [ ] `loupe top <field> ./logs [filter]` — value counts, descending
- [ ] `--limit` and a long tail summarised as "and N more", never truncated
      silently
- [ ] Works on promoted columns and JSON-bag fields alike
- [ ] Unknown field errors with a suggestion, like every other field reference
- [ ] UI: click-to-facet on any field value
- [ ] Percentages alongside counts, since "412 of 33,000" reads differently
      from "412 of 500"

---

## EC006 — Aggregations in the DSL

**Status: not started.** People drop to `loupe sql` mainly for counts, p99s and
rates. A thin layer over the AST → SQL compiler keeps them in the fast lane.

- [ ] `stats count() by level`, `stats p99(latency_ms) by path`
- [ ] Functions: `count`, `sum`, `avg`, `min`, `max`, `p50`, `p95`, `p99`
- [ ] Compile through the existing AST to parameterised SQL — never string
      concatenation, however small the change looks
- [ ] Round-trip test per aggregate form: `parse(render(ast)) == ast`
- [ ] Aggregating a non-numeric field must error clearly, not return zeroes
- [ ] Time bucketing — `by bin(1m)` or similar — since rate-over-time is most of
      the value
- [ ] `docs/FILTER-DSL.md` grammar updated

**Watch:** this is where a filter language turns into a query language. Keep it
to the shapes people actually type; `loupe sql` remains the escape hatch.

---

## EC007 — Window compare / diff

**Status: not started.** *"What is different between the healthy window and the
incident window?"* Pairs naturally with EC002 and is a genuine root-cause
accelerator no grep workflow offers.

- [ ] `loupe diff ./logs --before <window> --after <window>`
- [ ] Fields, values, and message patterns present in one window and not the
      other, plus rate changes for those in both
- [ ] Rank by "most surprising", not raw delta — a field that doubled from 2 to 4
      is noise next to one that went 0 → 300
- [ ] Reuse EC002's templates for the pattern half
- [ ] State both windows in local *and* UTC, like every other time output
- [ ] Handle windows of unequal length — compare rates, not counts, and say so

---

## EC008 — Broaden intake

**Status: not started.** Parsers are the contribution surface, so this doubles as
community fuel. Each parser is ~100 lines and a fixture.

**Compression.** `walk.go:42` currently skips `.zst`, `.bz2`, `.xz` outright;
only gzip is handled. zstd is everywhere in modern log rotation.

- [ ] zstd — check the licence and binary-size cost before adding a dependency;
      `CLAUDE.md` requires asking first
- [ ] bzip2 and xz (stdlib has bzip2; xz does not)
- [ ] Remove each from the skip list only once it actually reads
- [ ] Compressed files are not `Tailable` — EC001 already assumes this, keep it true

**Formats.**

- [ ] journald JSON export
- [ ] Docker `json-file`
- [ ] CRI / Kubernetes container log format
- [ ] Golden fixture each, messy per `CLAUDE.md`: blank lines, a truncated final
      line, mixed timestamp formats, at least one malformed record

---

# Tier 3 — nice, but watch for scope creep

## EC009 — Ad-hoc regex field extraction

**Status: not started.** Query-time extraction for fallback-parsed lines — pull
`latency_ms` out of an unstructured message without writing a parser. Makes
best-effort formats first-class.

- [ ] `--extract 'latency_ms=took (\d+)ms'` or an equivalent DSL term
- [ ] Extracted fields filterable and aggregatable like any other
- [ ] Anchor the cost: this runs per row, so it must not turn a 500ms query into
      a 30s one. Benchmark before shipping
- [ ] A pattern that matches nothing must say so rather than returning empty

**Watch:** the honest alternative is usually "write the parser", which is ~100
lines and helps everyone. Do not let this become the reason nobody contributes
parsers.

## EC010 — Self-contained HTML report export

**Status: not started.** Alongside the existing md/json/zip handoff: a single
double-clickable file with the timeline, the records, and the raw lines.

- [ ] One file, no external requests — same constraint the UI already meets
- [ ] Same plan as the display, so the export cannot differ from what was on
      screen (`docs/HANDOFF.md` already requires this)
- [ ] Carries every disclosure: assumed timezones, unparsed counts, truncation
- [ ] Respects `--redact`

## EC011 — Query history recall

**Status: not started.** Shell-style recall of previous filters in the CLI and
TUI. Explicitly **not** saved searches in the UI — that direction leads to
accounts and sync, which the project does not have.

- [ ] History file under the existing config directory
- [ ] Up-arrow recall in the TUI; a `loupe history` listing for the CLI
- [ ] Per-directory rather than global, since filters are data-specific
- [ ] Bounded size, no unbounded growth in a dotfile

---

## Notes

- Items are numbered `ECnnn` and get a branch of the same name. Sub-tasks are
  `ECnnn.n` for anything worth its own commit.
- Update the status table at the top when an item moves. It is the part people
  actually read.
- Three Tier 3 entries were reconstructed from a partially garbled source; the
  intent is captured but check EC009–EC011 against what you meant.
