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
| [EC002](#ec002--pattern-clustering--message-grouping) | Pattern clustering / message grouping | 1 | **done** |
| [EC003](#ec003--first-class-tracerequest-correlation) | Trace / request correlation | 1 | **done** |
| [EC004](#ec004--wire-up-stdin-streaming) | Wire up stdin streaming | 1 | **done** |
| [EC005](#ec005--faceted-breakdowns--top-n) | Faceted breakdowns / top-N | 2 | not started |
| [EC006](#ec006--aggregations-in-the-dsl) | Aggregations in the DSL | 2 | not started |
| [EC007](#ec007--window-compare--diff) | Window compare / diff | 2 | not started |
| [EC008](#ec008--broaden-intake) | Broaden intake | 2 | not started |
| [EC009](#ec009--ad-hoc-regex-field-extraction) | Ad-hoc regex field extraction | 3 | not started |
| [EC010](#ec010--self-contained-html-report-export) | Self-contained HTML report export | 3 | not started |
| [EC011](#ec011--query-history-recall) | Query history recall | 3 | not started |
| [EC012](#ec012--fuzz-the-parsers-and-the-dsl-lexer) | Fuzz the parsers and the DSL lexer | 1 | not started |
| [EC013](#ec013--benchmarks-for-the-unmeasured-performance-commitments) | Benchmarks for unmeasured perf commitments | 1 | not started |
| [EC014](#ec014--rotation-by-rename-test-coverage) | Rotation-by-rename test coverage | 1 | not started |
| [EC015](#ec015--exit-codes-as-a-scripting-contract) | Exit codes as a scripting contract | 2 | not started |
| [EC016](#ec016--loupe-fields-field-discovery) | `loupe fields` field discovery | 2 | not started |
| [EC017](#ec017--column-projection-on-output) | Column projection on output | 2 | not started |
| [EC018](#ec018--flag-subtraction-pass-before-v1) | Flag subtraction pass before v1 | 2 | not started |
| [EC019](#ec019--cmdloupe-test-coverage) | `cmd/loupe` test coverage | 2 | not started |
| [EC020](#ec020--decide-about-windows) | Decide about Windows | 3 | not started |
| [EC021](#ec021--fix-duplicate-24-in-docsfilter-dslmd) | Fix duplicate §2.4 in FILTER-DSL.md | 3 | not started |

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

**Status: done.** All four stages complete and tested. Work is on branch
`EC002`, cut from `main` after EC001 merged.

The highest-leverage triage feature: *"34,000 lines → 12 distinct templates, and
this one is new in the last 5 minutes."*

Collapses `user 4821 timed out` and `user 9903 timed out` into one pattern with a
count, so the anomaly is visible in a wall of noise. Pure read-only computation
over data already in DuckDB, and precisely what grep cannot do.

**Measured on the 28.7 MB benchmark corpus:** 186,452 records carrying 3,144
distinct messages collapse to 922 templates, and the top 17 templates cover 99%
of the records. The 900-template tail is the malformed and truncated lines, each
its own shape — which is the rule working, not failing.

### Where templating runs — decided

At ingest, stored as `pattern` and `pattern_id` columns, rather than derived per
query. Three reasons, in order of weight:

1. `pattern:<id>` has to compile to an ordinary predicate. Deriving templates in
   SQL would mean a second implementation of the masking rules that could
   disagree with the Go one, which is the "tests against a mocked store" failure
   in a different costume.
2. `--new-since` needs ids that are stable across runs. A hash of a
   deterministic template is stable by construction, with nothing stored between
   runs to go out of date.
3. Grouping becomes `GROUP BY pattern_id`, which DuckDB does for free.

The cost is `IngestVersion` 5 → 6, so every cached ingest is re-read once.

### EC002.1 — The templater and the stored columns — **done**

- [x] `internal/pattern` — mask value-shaped tokens, group by the result
- [x] Numeric ids, UUIDs, IPs, paths, quoted strings, timestamps, and opaque
      hex ids all recognised as variable
- [x] `pattern` and `pattern_id` columns, computed in `Ingester.Add`
- [x] `IngestVersion` 5 → 6
- [x] Live records get their pattern too, through the follow staging path
- [x] Unparsed records template their raw line, not their empty message
- [x] Stable ids: same template text always yields the same id, on any machine
- [x] Table-driven tests per masking rule, plus explicit over-collapse guards
- [x] `BenchmarkIngest` added — there were no benchmarks in the repo before

**Paths are masked per segment, not wholesale.** `POST /api/orders/2291` becomes
`POST /api/orders/<num>`, but `/api/cart` and `/api/checkout` stay distinct.
Collapsing a path to `<path>` would have merged every endpoint into one template,
and which endpoint is failing is usually the entire finding.

**Only the first line of a message is templated.** A Log4j record carries its
stack trace in the message and every stack trace differs somewhere, so
templating the whole thing gave every exception a template of its own — no
templates at all for exactly the records that matter most.

**Ingest cost, measured** (28.7 MB, 212,878 lines, six formats, seed 42, five
runs each):

| | median | throughput | allocs |
|---|---|---|---|
| before | 4.95 s | 5.8 MB/s | 14.04 M |
| after | 5.47 s | 5.2 MB/s | 14.78 M |

**+10.5%.** Isolated by writing the columns empty, ~70% of that is the templater
itself and ~30% is DuckDB carrying two more columns. First cut was +19%: the
masking rules were calling `strings.Split` inside the IPv4 and clock checks, on
every token of every record. Rewriting those to scan in place, returning the
original string when a line has nothing to mask, and rejecting plain words in one
pass took a plain message from 240 ns to 45 ns and zero allocations.

Not optimised further on purpose. The next step would be computing a token's
shape once and gating each rule on it, and there is no complaint yet that
justifies the complexity.

### EC002.2 — `loupe patterns` — **done**

- [x] `loupe patterns ./logs [filter]` — template, count, first seen, last seen,
      example line, sources it appears in, all on `session.PatternSet`
- [x] `--limit`, `--all`, and a tail summarised as "N templates more not shown,
      covering N records", never truncated silently
- [x] `--new-since <window>` — lists only templates with no records before the
      cutoff, and counts the established ones it left out
- [x] Both zones printed for the cutoff, like every other time output
- [x] Golden test over a blaster-generated demo corpus: 49 templates, checked in
      at `internal/session/testdata/patterns.golden`, regenerate with `-update`
- [x] Templates on stdout, every caveat on stderr, so a pipe cannot swallow the
      disclosures

`session.Patterns` is the shared middle, so the API and TUI in stage 4 call the
same code rather than growing their own.

**`--new-since` counts back from the same anchor `last:` uses** — the newest
record, unless the session was opened `--relative-to now`. `query.TimeContext.
Anchor` and `query.ParseDuration` were exported for it rather than reimplemented,
so `--new-since 15m` and `last:15m` cannot drift apart about which records are
recent or which units exist.

**The listing separates the templates that came from unreadable lines.** On the
benchmark corpus, 184,969 parsed records collapse to 85 templates while 1,499
lines no parser understood produce 846 — a broken line genuinely is its own
shape. Without saying so, "931 templates" reads as the collapse rule having
failed rather than as the data saying something true. The footer states the
split and offers `parsed:true` to exclude it.

**Golden test corpus is generated, not the `demo/` directory.** `demo/` is
gitignored, so a test reading it would pass on a machine that had run
`make demo` and skip everywhere else. The blaster at a fixed seed gives the same
six formats and the same deliberate noise, reproducibly, in CI.

`render.Commas` was exported and the duplicate copies in `internal/tui` and the
handoff renderer removed — the pattern listing was the third caller, which is
the threshold `CLAUDE.md` sets for abstracting rather than copying again.

Two bugs found while building it, both of the quiet kind. `string_agg(DISTINCT
source, '\x1f')` does not mean what it looks like: DuckDB takes the literal four
characters, so every multi-source template would have reported one run-on name
rather than a list — `chr(31)` is the honest way to say it. And the footer was
built from `plural()`, which returns only the noun, so it read "templates
covering records" until the numbers were put back.

### EC002.3 — `pattern:<id>` as a DSL term — **done**

- [x] A template expands back to exactly its matching records — a test asserts
      the listed count and the `pattern:<id>` count agree for every template
- [x] Compiles to a parameterised predicate on `pattern_id`
- [x] Round-trip tests: `pattern:<id>`, a short id, negation, a comma list, and
      `pattern:none` all added to `TestRoundTrip`
- [x] An unknown id errors with the nearest ids by prefix, never an empty result
- [x] Short ids resolve like git short hashes; an ambiguous one lists candidates
- [x] `docs/FILTER-DSL.md` §4.1 written

**Id resolution lives in the session, not the compiler**, because resolving one
needs the database and `internal/query` deliberately never touches one. It uses
bounded prefix lookups rather than loading every id into `query.Schema`: a
corpus where every line is unique has as many templates as records, and holding
that list in memory to validate one term would be a poor trade.

**An unknown id is an error, unlike an unknown source.** A source name is
something the user knows from outside the data, so `source:nginx` matching
nothing is a real answer to a real question. A template id only ever comes from
a `loupe patterns` listing of this same data, so an id that is not present is a
typo or a stale paste. The three failures are told apart and corrected
differently: an id that does not exist suggests the nearest by prefix, a short
id matching several lists them, and a value that is not hexadecimal is told that
`pattern:` takes an id rather than a template's text.

Resolving a short id copies the term rather than rewriting it, so the query
reported back to the user stays the one they typed. A test pins that.

### EC002.4 — The UI — **done**

- [x] `GET /api/patterns` — after the CLI existed, per invariant 2
- [x] Pattern list as a left rail, click to filter
- [x] Playwright coverage — eight specs against the real binary
- [x] `ARCHITECTURE.md` §5 and §6, and `loupe serve --help`, updated

The endpoint returns `session.PatternSet` unchanged rather than a hand-built
subset, so the rail and `loupe patterns` cannot disagree about what a listing
contains — including what a limit hid, which the rail states rather than
stopping quietly.

**The rail is off until asked for**, like the live tail. It costs a grouping
query and takes width from the message column, and the one screen in
`ARCHITECTURE.md` §6 is worth protecting from anything permanent. `p` toggles
it. Clicking a template writes a real `pattern:<id>` term into the filter box,
so the interaction teaches the syntax; clicking the selected one clears it.

A Playwright test asserts the count beside a template and the record count after
clicking it are the same number. That is the property that makes the rail a
summary of what it selects rather than of something else.

**Bug found by looking at it in a browser: templates could contain NUL bytes.**
The corpus is full of lines the blaster corrupted with NULs, and a NUL renders
as nothing in a terminal and as a replacement box in a browser — so
`POST /api\0\0/orders/1` read as a spacing bug in loupe rather than as damage
in the log. `internal/query` already had a comment describing exactly this trap
for field names. Control characters are now masked as `<ctl>` in the templater
itself, so the CLI, the TUI, the API and the rail all benefit; a run collapses
to one mask, and the position of the damage is preserved so corruption in two
different places stays two templates. This changed template ids, so the golden
file was regenerated and its diff reviewed.

**Watch:** the failure mode is over-collapsing — merging two genuinely different
errors into one template hides the incident. Prefer too many templates to too
few, and make the collapse rule inspectable. Stage 1 holds this line: only
value-shaped tokens are masked, a bare word is never touched, and the template
text shows exactly what was collapsed. `TestWordsAreNeverMasked` pins it.

---

## EC003 — First-class trace / request correlation

**Status: done.** All three stages complete and tested. Work is on branch
`EC003`, cut from `main` after EC004 merged.

The demo already brags that one `trace_id` runs through all six sources; that is
currently the pitch, not a feature.

Delivers the README's exact promise: watch the pool exhaust, then the app error,
then Nginx 502, in the order it happened.

**What the data actually looks like**, measured on the demo corpus: `trace_id`
reaches three of the six sources. Nginx, Postgres and syslog carry no
correlation id at all in their formats. That is not a gap in the fixture, it is
the normal state of a real system, and it is why the third checklist item
matters more than it first reads: a trace view that lists three sources must not
let anyone conclude the request never reached the other three.

### EC003.1 — Detection and `loupe trace` — **done**

- [x] `loupe trace <id> ./logs` — one request's timeline across every source
- [x] The gap between consecutive hops, with the largest one marked
- [x] The correlation field is detected and the choice is stated; `--field`
      overrides it
- [x] A trace present in some sources and absent from others does not imply the
      request skipped a service
- [x] Records with no timestamp are ordered last, counted, and never dropped
- [x] Tests: ordering, gaps, undated hops, silent vs blind sources, detection
      by coverage, no correlation field at all, awkward ids

```
  00:16:00.000          auth-svc       info  token validated
▸ 00:16:00.632  +632ms  checkout-api   info  request completed
  00:16:00.700   +68ms  payment-worker error PaymentGatewayException: read timed out after 3000ms …

Span 700ms, of which 632ms waiting before checkout-api.
access, postgresql, syslog never record trace_id, so this trace cannot say
whether the request reached them.
```

**Silent and blind sources are different facts, and conflating them is the trap
this stage exists to avoid.** A source that records correlation ids and has none
for this trace probably did not handle the request. A source that never records
them — Nginx combined has nowhere to put one — may have handled it and simply
cannot say. Reported as one category, the reader concludes a request skipped
services it went straight through.

**Detection is by coverage, not by list order.** The candidate order only breaks
ties, so a `request_id` on three records cannot outrank a `trace_id` on three
hundred. Other candidates present are named, so a wrong guess is visible.

The correlation field is resolved through the ordinary filter path rather than
by reading the schema directly, so a promoted column and a key still in the JSON
bag are found the same way. The filter term is built from the AST rather than
pasted together, so an id containing a quote or a space still produces a term
that parses back to what was meant.

`loupe trace` reads the whole dataset before answering, including a piped one:
it cannot know which sources stayed silent until every source has been read.

### EC003.2 — Handoff export of one trace — **done**

- [x] `loupe trace <id> ./logs --handoff incident.md`
- [x] The timeline sits above the record table, in both zones, with the longest
      wait marked
- [x] Carries the same disclosures as the terminal view, plus everything an
      ordinary extract carries: assumed zones, unparsed counts, truncation
- [x] Tests at both levels — the extract's shape, and the markdown it renders to

A trace extract is the ordinary handoff for the records the trace matched, with
the timeline attached, so it cannot show records the same filter would not. That
is the property `docs/HANDOFF.md` asks for, and building a second record path
for traces would have quietly broken it.

**Which sources could not answer travels with the extract.** The receiver is
reading a claim about where a request went, in a channel, without the data in
front of them — they cannot see that Nginx was never able to be asked. An
extract naming three services and staying quiet about the other three misleads
by omission, which is the one thing a handoff must not do.

`runHandoff` was split so the extract-building and the write-and-rename are
separate: a trace extract reuses the atomic write rather than copying it, and
both paths share one `handoffOptions`, so a trace extract and a filter extract
cannot be built to different rules.

`humanGap` in the CLI and the same logic needed by the extract became one
exported `session.HumanDuration` rather than a second copy.

### EC003.3 — The trace view in the browser — **done**

- [x] `GET /api/trace` and `GET /api/trace-field`, after the CLI existed
- [x] A **→ trace** button on the correlation field in a record's detail
- [x] The gap between hops is drawn as a bar scaled to the longest wait in the
      trace, and the longest row is highlighted
- [x] The footer says which sources could not answer, which stayed quiet, and
      how many hops carry no timestamp
- [x] Playwright coverage — six specs against the real binary

The endpoint returns `session.Trace` unchanged, with silent and blind computed
on the Go side, so the browser cannot re-derive the distinction differently from
the terminal.

`/api/trace-field` exists so the affordance appears only where it would work.
Data with nothing correlation-shaped in it gets no trace button rather than a
button that opens an empty panel, and that is an ordinary answer rather than an
error.

**The bar is scaled to the longest wait in the trace, not to an absolute
duration.** A four-millisecond trace and a four-second one would otherwise look
identical, which is the opposite of the point.

**Regression caught by the existing suite:** the trace panel registered a
capture-phase Escape listener that stayed attached while the panel was closed,
so it swallowed Escape before the rest of the app saw it — and Escape is also
how the filter is cleared and the help panel dismissed. Two `ui.spec.js` tests
that had nothing to do with traces went red, which is exactly what they are for.
The listener is now scoped to an open trace.

---

## EC004 — Wire up stdin streaming

**Status: done.** Both stages complete and tested. Work is on branch `EC004`,
cut from `main` after EC002 merged.

`internal/source/file.go` defined `Stdin` and its comment said it was for
``kubectl logs -f api | loupe``. `NewStdin` had no callers outside tests: the
type was complete and unreachable, so the documented pipeline did not work.

Opens the whole container ecosystem with no new concepts and no daemon:
`kubectl logs -f`, `docker logs`, `journalctl -f` all pipe straight in.

### EC004.1 — stdin as a real source — **done**

- [x] Detect a piped stdin and use it when no path is given
- [x] Explicit `-` as a path argument, which composes with real paths
- [x] Never block on an interactive terminal waiting for input nobody is typing
- [x] Uncacheable by design — the status line says so in its own words rather
      than claiming it "re-read the log files", which a pipe has none of
- [x] A stream has no size, and nothing divides by it
- [x] A stream is not `Tailable` and is never asked to seek — the follower skips
      it, and a test pins both
- [x] Piped gzip works, detected from the magic bytes
- [x] Tests: piped fixture, empty stdin, stdin plus a directory, stdin closed
      mid-record, gzip, peek-does-not-consume, follower-skips-a-stream

**Bug found: format detection was eating the first two hundred lines of every
stream.** `parserFor` opened the source to sample it, then `loadOne` opened it
again to read it. A file can be opened twice; a stream cannot, so the sample
was consumed and lost. Piping 500 lines ingested 157 — and nothing said so,
which is precisely the confident wrong answer this project exists to avoid.

Sources can now be peeked instead: `source.Peekable` returns leading bytes
without consuming them, `Stdin` wraps the pipe exactly once so every `Open`
returns the same reader at the same position, and detection reads through a
peek. Gzip is decompressed inside that single wrap, so detection scores plain
text rather than compressed bytes — which is why the checklist's claim that
piped gzip "already works" was wrong: it produced zero records.

`--follow` on a stream is refused rather than silently polling nothing:
standard input is already live, and ends when the writer closes it.

### EC004.2 — Streaming render — **done**

- [x] Render as records arrive rather than after EOF
- [x] A stream's filter compiles once, against the records that have arrived,
      and an unknown field says so rather than returning nothing
- [x] Finite sources are read before streams, so a pipe that never ends cannot
      starve a directory listed beside it
- [x] Ctrl-C ends a stream the way closing the pipe does, and exits zero
- [x] Batch commands — `patterns`, `histogram`, `sql`, `sources`, `tui`,
      `serve` — read the stream to the end first, and say they are doing it
- [x] Tests: every record once across batch boundaries, filtering, bag-field
      filtering, unknown field, finite-source ordering, cancellation, empty pipe

**Two things had to change below the streaming layer, and both were bugs in
their own right.**

`parse.ReadAll` held every record until the *next* line arrived, so that a
continuation line could be attached to it. On a file that costs nothing. On a
stream it meant always showing the record before last — a service logging once
a minute would look permanently stuck one record behind. Only Log4j has
continuations, so a parser with no `Continuer` now flushes immediately; the
lookahead is kept for the formats that actually need it, because a stack trace
emitted before its own trace would be worse than a late one.

`bufio.Peek` blocks until it has *exactly* the number of bytes asked for.
Format detection asked for its full window, which on a live pipe never arrives,
so `kubectl logs -f` on a quiet pod hung in detection before reading a single
record. `Stdin.Peek` now waits for the first byte and takes what came with it.
The cost is that a slow producer is detected from less text than a file would
be; `--parser` is the escape hatch for a format that needs more than its
opening lines.

**Streaming gives up promotion, and says so.** Promotion rewrites the logs
table, which is not something to do to a table an appender is still writing
into. Every field stays queryable — read from the JSON bag instead of a typed
column — so this is a performance difference rather than a correctness one, and
the status line states it. A *drained* stream does get promoted, because by
then the read has finished, which is why `cat app.log | loupe patterns` gets
the same typed columns a file would.

**Also fixed, and it belongs to EC001:** the table renderer reprinted its header
for every batch, so `--follow` produced a stack of one-row tables rather than a
log. `render.Options.Continuous` prints the header once, and follow mode now
sets it.

`--handoff` is refused on a stream: an extract describes a finished read, and
silently covering "whatever had arrived" would be the wrong kind of honest.

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
