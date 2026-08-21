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
| [EC005](#ec005--faceted-breakdowns--top-n) | Faceted breakdowns / top-N | 2 | **done** |
| [EC006](#ec006--aggregations-in-the-dsl) | Aggregations in the DSL | 2 | **done** |
| [EC007](#ec007--window-compare--diff) | Window compare / diff | 2 | **done** |
| [EC008](#ec008--broaden-intake) | Broaden intake | 2 | **done** |
| [EC009](#ec009--ad-hoc-regex-field-extraction) | Ad-hoc regex field extraction | 3 | not started |
| [EC010](#ec010--self-contained-html-report-export) | Self-contained HTML report export | 3 | not started |
| [EC011](#ec011--query-history-recall) | Query history recall | 3 | not started |
| [EC012](#ec012--fuzz-the-parsers-and-the-dsl-lexer) | Fuzz the parsers and the DSL lexer | 1 | done |
| [EC013](#ec013--benchmarks-for-the-unmeasured-performance-commitments) | Benchmarks for unmeasured perf commitments | 1 | not started |
| [EC014](#ec014--rotation-by-rename-test-coverage) | Rotation-by-rename test coverage | 1 | not started |
| [EC015](#ec015--exit-codes-as-a-scripting-contract) | Exit codes as a scripting contract | 2 | not started |
| [EC016](#ec016--loupe-fields-field-discovery) | `loupe fields` field discovery | 2 | **done** |
| [EC017](#ec017--column-projection-on-output) | Column projection on output | 2 | not started |
| [EC018](#ec018--flag-subtraction-pass-before-v1) | Flag subtraction pass before v1 | 2 | not started |
| [EC019](#ec019--cmdloupe-test-coverage) | `cmd/loupe` test coverage | 2 | not started |
| [EC020](#ec020--decide-about-windows) | Decide about Windows | 3 | not started |
| [EC021](#ec021--fix-duplicate-24-in-docsfilter-dslmd) | Fix duplicate §2.4 in FILTER-DSL.md | 3 | not started |
| [EC022](#ec022--a-control-character-in-a-field-name-breaks-the-query) | Control character in a field name breaks the query | 1 | not started |
| [EC023](#ec023--a-negated-filter-is-unusable-without---) | Negated filter unusable without `--` | 2 | not started |
| [EC024](#ec024--symlinked-log-files-are-skipped) | Symlinked log files are skipped | 2 | not started |

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

**Status: done.** Both stages complete and tested. Work is on branch `EC005`,
cut from `main` — note that EC003 was not merged when this branched, so the two
both add a command to `root.go` and will conflict trivially there on merge.

Answers the most common triage question — *"which endpoints are 500ing?"* —
without dropping to SQL. A `GROUP BY` DuckDB does for free.

### EC005.1 — `loupe top` — **done**

- [x] `loupe top <field> ./logs [filter]` — value counts, descending
- [x] `--limit`, `--all`, and a tail summarised as "N values more not shown,
      covering N records", never truncated silently
- [x] Works on promoted columns, built-in columns, and JSON-bag fields alike
- [x] An unknown field errors with a spelling suggestion, like every other field
      reference
- [x] Percentages alongside counts, with the denominator stated
- [x] Tests: ordering, stability, shares summing to one, absent records,
      truncation arithmetic, empty values, unknown fields, empty results

```
2,936   20.3%  ████████████████████████  /healthz
2,905   20.1%  ███████████████████████   /api/orders/<num>
    2    0.0%  █                         <ctl><ctl>/api/cart

10 values of path across 14,480 records.
43,630 records matched the filter but carry no path, so they are outside the
percentages above (path:none finds them).
```

**The denominator is the records that carry the field, and it is stated.** A
share is meaningless without one, and the two obvious choices differ: 300 of 412
records with a path is 72.8%, while 300 of 500 matched records is 60%. Values
sum to one so the list reads as a distribution, and the records missing the
field are reported separately rather than folded into the denominator where they
would silently shrink every percentage.

**A field present but empty is a value, not an absence.** It renders as
`(empty)`, because a blank cell reads as a rendering fault rather than as the
data.

`query.Schema.Column` was exported so the breakdown resolves a field through the
same code the filter compiler uses. That is what makes the third and fourth
checklist items free: a promoted column, a built-in one and a bag key are all
found the same way, and an unknown name produces the identical error with the
identical spelling suggestion. Building a second resolver here would have been
the obvious way to let a facet and a filter disagree about what a name means.

Control characters in a value are masked as `<ctl>`, the same decision EC002
reached for template text and for the same reason: a NUL renders as nothing in a
terminal, so a corrupted path looked like a spacing bug in loupe rather than
damage in the log.

### EC005.2 — Click-to-facet in the browser — **done**

- [x] `GET /api/top`, after the CLI existed
- [x] A **% top** button on every field in an expanded record
- [x] Percentages, the denominator, and the absent count all travel with the
      response and are stated in the panel
- [x] Clicking a value filters on it; the absent count offers `field:none`
- [x] Playwright coverage — seven specs against the real binary

The endpoint returns `session.TopSet` unchanged, so the browser never recomputes
a share. A percentage the UI derived itself could disagree with the one
`loupe top` prints, and there would be no way to tell which was right.

**The affordance is on the field, not the value.** A record showing
`path=/api/cart` is the place you ask "what paths are there", so the button
breaks down the field and the existing click-to-filter still handles the value.
Putting a breakdown on the value would have answered a question nobody asks.

**Bars scale to the largest value, not to the total.** Scaling to the total
flattens every bar as soon as one value dominates, which is the case most worth
seeing.

The Escape listener is scoped to an open panel from the outset — a
capture-phase listener left attached while closed swallows the key that clears
the filter, which is the regression EC003 shipped and the existing suite caught.

---

## EC006 — Aggregations in the DSL

**Status: done.** Both stages complete and tested. Work is on branch `EC006`,
cut from `main`.

People drop to `loupe sql` mainly for counts, p99s and rates. A thin layer over
the AST → SQL compiler keeps them in the fast lane.

### EC006.1 — The grammar and the compiler — **done**

- [x] `stats count() by level`, `stats p99(latency_ms) by path`
- [x] Functions: `count`, `sum`, `avg`, `min`, `max`, `p50`, `p95`, `p99`
- [x] Several aggregates and several groupings, comma separated
- [x] Time bucketing — `by bin(1m)` — as a grouping, not a separate clause
- [x] Compile through the existing AST to parameterised SQL
- [x] Round-trip test per aggregate form: `parse(render(ast)) == ast`
- [x] An unknown aggregate errors with a spelling suggestion, like an unknown
      field

```
query   := term* [stats]
stats   := 'stats' agg (',' agg)* ['by' groupkey (',' groupkey)*]
agg     := aggfunc '(' [key | '*'] ')'
groupkey:= key | 'bin' '(' duration ')'
```

**Aggregation is part of the filter, not a subcommand.** `loupe ./logs
'level:>=error stats count() by path'` is one question, and the same string
works on the command line, in a saved query, and in the UI's filter box. A
`loupe stats` command would have been a second place to express a filter and a
second thing to keep in step with the first.

**The lexer never learned about parentheses.** `count(latency_ms)` arrives as a
single word and is taken apart by the parser, which is the same division of
labour that leaves a time expression whole for the resolver. Making `(`
structural in the lexer would have been the obvious move and would have broken
every value containing one — `/api/v1/(id)`, `message~(GET)`, and most stack
traces. The cost is one lookahead for the `p99("odd key")` form, where the lexer
ends the bare word at the quote and the closing bracket arrives as its own token.

**`stats` is reserved where a term begins.** A bare `stats` is a clause with
nothing in it and errors saying how to search for the word instead: `"stats"`.
The parser marks such a free-text value `Quoted` at parse time, exactly as it
already does for a value starting with an operator, so rendering puts the quotes
back and the round trip holds. A field *called* stats is unaffected — `stats:5`
and `stats~high` are ordinary field terms, because the keyword only counts where
the word is not being used as a name. This is the same bargain the time keywords
already make, and it is the price of putting aggregation in the language rather
than behind a flag.

**A bin is a whole number of seconds.** The duration grammar's smallest unit is
the second, so `bin(0.5s)` could not be rendered back to something that parses
to the same width — and a filter that changes when the UI writes it back into
the box is the failure the round-trip rule exists to prevent. Refusing it is one
error message; accepting it would have been a silent drift. `bin(60s)` renders
as `bin(1m)`, which is the same query, not a second spelling.

**Nothing in a stats clause is a value, so nothing in it needs a parameter.**
Field names resolve to column expressions through `Schema.resolve` — the same
resolver a filter uses, so a facet, a filter and an aggregate cannot disagree
about what a name means — and a bucket width is a number the compiler computed.
`StatsSQL` therefore has no `Args` field at all, which is what lets the counting
query reuse the grouping conditions inside a `FILTER` without binding the
filter's arguments a second time. The first draft did not, and DuckDB said
"have 2 want 4".

`FuzzParse` and `FuzzCompile` were extended with the new shapes and run for
15.5M executions across the two, with the round-trip comparison widened from
`Query.Terms` to the whole `Query` so a clause that did not survive rendering
would be caught.

### EC006.2 — Running it — **done**

- [x] `Session.Stats` over a real DuckDB instance, through the same plan as
      every filter
- [x] Aggregating a non-numeric field errors clearly, naming a sample value
- [x] Every record the summary could not place is counted and named
- [x] Buckets anchored to local midnight in the display timezone, stated in both
      zones
- [x] `docs/FILTER-DSL.md` §1 grammar and a new §10; `ARCHITECTURE.md` §3.5;
      `README.md`
- [x] Tests: values, ordering, headings, exclusions, truncation, the non-numeric
      error, the partial-numeric note, bucket anchoring in a half-hour zone, a
      clock change inside the window, a window crossing midnight

```
PATH              COUNT()  P99(LATENCY_MS)
/api/orders/2291  560      4961.2
/api/cart         547      4963.44
/healthz          544      4971.48

6 groups over 2,700 of 3,524 matching records.
824 records matched the filter but carry no path, so they are in no group (path:none finds them).
```

**`Session.Plan` refuses an aggregation; `Session.PlanAggregate` accepts one.**
Every caller that lists records already went through `Plan` — the HTTP API, the
TUI, follow mode, handoff, `loupe top`, `loupe histogram`, `loupe patterns`,
`loupe trace` — so one edit gave all of them a clear refusal instead of the
silent drop that would otherwise have shown a listing answering a different
question from the one typed. Only `runDefault` opts in. The alternative was
eight call sites that each had to remember, which is eight chances to forget.

**Buckets are anchored to local midnight in the display timezone, not to the
Unix epoch.** Epoch alignment is the free option and it is wrong here: it puts
every `bin(1h)` boundary half an hour out in India and three quarters of an hour
out in Nepal, which is precisely the offset arithmetic `docs/FILTER-DSL.md` §2.3
exists to spare people. The anchor is found with `time.Date` in the location, so
it comes from the tz database rather than from arithmetic, and it is printed in
both zones under the table.

A bucket is a fixed width of *real* time, so after a clock change the boundaries
stop lining up with the local clock they were anchored to. That is what a bucket
of elapsed time has to do; hiding it would make the column look shifted for no
visible reason, so a window containing a transition says so, names both zones,
and gives the offset the boundaries moved by.

**Empty buckets are counted, not filled.** A bucket with nothing in it is not a
group and has no row, so a rate table can put 14:04 directly above 14:06 and
read as continuous. Generating the missing rows was the other option and it
scales badly — `bin(1s)` over a week is 604,800 rows, most of them empty, and
they would spend the whole `--limit`. Counting them costs three columns on a
scan that was already happening and says the same thing: *"1 bucket between the
first and the last holds no matching record — `loupe histogram` draws the gaps."*

**A record with no value for a grouping field belongs to no group.** It is
excluded and counted, with `path:none` offered — the same decision EC005 reached
for `top`'s absent count, for the same reason. Collecting them into a nameless
row was the alternative, and a blank cell in a table reads as a rendering fault
rather than as the data.

**Aggregating a field that holds no numbers is an error.** `avg(path)` over
`TRY_CAST` produces a column of NULLs, which reads as "no data" rather than
"wrong question" — the confident wrong answer this project exists to avoid. One
probe query per aggregation counts the values, counts the ones that cast, and
picks up a sample, so the error can say *"path does not hold numbers — none of
its 26,115 values is one, e.g. "/api/checkout""* and then offer the two things
that would have worked: `count(path)`, and `loupe top path`. A field that is
numeric for *most* records is not an error but is never silent: the values that
could not be read are counted, because an average over four fifths of the values
is not an average over all of them.

**Timestamps are deliberately not aggregable.** `min(ts)` would be useful and it
is one branch away, but it drags `sum(ts)` and `p99(ts)` behind it as cases that
have to be refused individually. The status line already prints the range and
`loupe histogram` shows its shape.

**Rows come back as a `store.Result`,** so they go through the renderer every
other listing uses and `--format json`, `--format csv` and `--format ndjson`
work without a line of new code. The one addition is a row noun: the truncation
footer says "showing 5 of 6 groups", because calling a group a record would
misstate the size of what the limit cut.

**Ordering is by time when there is a bin, and by the first aggregate
otherwise.** A rate read in any other order is not a rate; without a bin, the
answer to "which is worst" belongs on the first line. Grouping columns break
ties so the same data always lists in the same order — a summary that reordered
itself between runs could not be compared against one taken a minute earlier.

**Table output now trims floats to twelve significant digits.** An interpolated
p99 of 4963.44 printed as `4963.4400000000005`, and a reader concludes the tool
is broken rather than that binary floating point is. Only the table format
changed; JSON, NDJSON and CSV still carry the exact double, because those are
read by machines that want the value that round-trips. This also tidies
`loupe sql` output, which had the same rough edge.

**No UI stage, because the checklist has none.** CLAUDE.md requires the CLI
first, not the UI eventually. A `stats` clause typed into the filter box comes
back as a 400 naming the clause and saying where summaries are printed; a server
test pins that, so the day the browser learns to render one it will be a
deliberate change rather than a silent behaviour flip.

**Watch:** this is where a filter language turns into a query language. Keep it
to the shapes people actually type; `loupe sql` remains the escape hatch. The
next thing someone will ask for is `sort` and `head`, and the answer should
probably stay no — `--limit` plus the ordering rule covers the cases that
matter, and a pipeline grammar is how this becomes a language nobody remembers.

---

## EC007 — Window compare / diff

**Status: done.** Both stages complete and tested. Work is on branch `EC007`,
cut from `main`.

*"What is different between the healthy window and the incident window?"* Pairs
naturally with EC002 and is a genuine root-cause accelerator no grep workflow
offers.

### EC007.1 — The comparison engine and the pattern half — **done**

- [x] `loupe diff ./logs --before <window> --after <window>`
- [x] Message patterns present in one window and not the other, plus rate
      changes for those in both
- [x] Rank by "most surprising", not raw delta
- [x] Reuse EC002's templates for the pattern half
- [x] State both windows in local *and* UTC, with their lengths
- [x] Handle windows of unequal length — compare rates, not counts, and say so
- [x] `--limit`, `--all`, and a stated count of what was cut

```
before  13:00:00–14:00:00 BST  =  12:00:00–13:00:00 UTC  ·  Thu 2026-08-13
        1h · 12,043 records
after   14:00:00–15:00:00 BST  =  13:00:00–14:00:00 UTC  ·  Thu 2026-08-13
        1h · 14,201 records

Volume went from 12,043 to 14,201 records (+18%). Everything below is ranked on what changed beyond that.

BEFORE   AFTER  CHANGE  WHAT
     0     312     new  pattern 9acf7d11  connection to <host> refused
   140   1,316    ×9.4  status=503
 1,204       0    gone  pattern 3ab1f0aa  cache warm complete
```

**A window is a filter expression, not a new syntax.** `--before 13:00-14:00`
goes through `Session.Plan` exactly as `13:00-14:00` does in a filter, so a bare
time lands on a day the data covers, a clock change inside the window is noted,
and both windows print in local and UTC through the same `Interval.Describe`
every other command uses. A filter given alongside intersects with each window
the way two written terms do, which is why `loupe diff ./logs 'source:nginx'
--before … --after …` needs no special handling. Inventing a window grammar here
would have been a second thing to learn and a second thing to get wrong about
timezones.

**Each side's counts are what the tool would print for that window alone.** Both
halves come from `Session.Patterns` over that window's plan, so the templates
are EC002's — computed once at ingest — and a comparison cannot disagree with
`loupe patterns ./logs '<window>'`. A second masking implementation here would
have been the obvious way to let the two drift.

**The ranking is a log-likelihood ratio, the G² statistic.** A raw delta cannot
tell 2 → 4 from 0 → 300; here the first scores 0.68 and the second 416, because
a doubling of two is exactly what noise looks like and an appearance out of
nothing is not. A large drop scores highest of all — 9,800 → 480 is 10,366 —
which is right, because a service that stopped saying what it always said is the
most informative thing in either window.

**Rates for display, shares for ranking.** These answer different questions and
the table answers both. Rates come from the windows' durations, because an hour
of healthy traffic against five minutes of an incident is a legitimate question
and 100 records means nothing without the span. The ranking apportions expected
counts by each window's *record count* instead — see EC007.2, where that
decision was forced.

**Counts when the windows are the same length, rates when they are not.** The
common case is two one-hour windows, where a count is what people think in and
the two are directly comparable. The moment the lengths differ the columns
switch to a rate, the unit is chosen so the largest number on screen is at least
one, and the footer states both lengths and the unit.

**An open-ended window is bounded by the data's own range, and says so.**
`--before before:14:00` has no length, and a rate needs one. Clamping to the span
the data covers is the only bound that means anything, and it changes no counts:
no record exists outside that span, so the clamped window selects exactly what
the unclamped predicate did. The resolved window is printed either way.

**Overlapping windows are reported, not refused.** A record in the overlap is
counted on both sides, which is what keeps each column equal to what that window
alone would report. Comparing a window with itself finds nothing, which is a
useful thing for the tool to be able to say.

### EC007.2 — Fields and values — **done**

- [x] Fields present in one window and not the other
- [x] Values of each field, compared and ranked on the same scale as templates
- [x] Identifier-like fields named rather than silently left out
- [x] Tests: classification, ranking, per-kind tallies, unequal windows,
      overlap, empty windows, clamping, determinism, suppression rules

**Apportioning surprise by duration was wrong, and the field half is what proved
it.** With rate-based ranking, a sixtyfold rise in traffic put `field level ×61`,
`field path ×61`, `field status ×61`, `field source ×61` at the top of the list —
one fact restated once per field, burying the single template that had actually
appeared. Expected counts are now apportioned by each window's record count, so
the score answers *"what is different about this window beyond there simply
being more of it"*. The change in volume is a single fact and is printed once,
above the table, as the thing everything below is measured against.

That change also removed a special case rather than adding one. A field every
record carries has a share of one in both windows, so it scores zero and drops
out under the same rule as everything else; an earlier version detected and
suppressed those explicitly, with its own footer line to explain the omission.
One rule applied to templates, fields and values alike is both smaller and
easier to state.

**A field with one value is dropped; its presence row survives.** The two rows
would carry identical counts and identical scores, sitting next to each other
saying the same thing twice. The field row is the one kept, because its absence
is the finding.

**Values are compared for fields with at most 500 distinct values.** Above that
a field is an identifier rather than a category, and *"trace_id=a91c40f2
appeared"* is true of every trace in the window — it is not a finding. The
ranking would sink them anyway, since a value seen once scores 1.4, but holding
several million of them in a map to find that out would break the bounded-memory
commitment. Presence is still compared, and the footer names every field left
out with its distinct count.

**One statement per window for the values, not one per field.** The window is
read into a CTE and each field unpivoted off it, so the cost is a single scan
however many fields there are. Field names reach SQL as bound parameters rather
than as text; the JSON path inside each expression is escaped by
`internal/query`, which is the same resolver a filter uses, so a facet, a filter
and a comparison cannot disagree about what a name means.

**A field whose name holds a control character is left out and named.** This is
not a comparison bug — see EC022 — but a comparison is the only thing that
references every field at once, so it is the only thing that meets it on
ordinary data. Failing the whole run because one field in a corrupted log has a
NUL in its name would be the wrong trade.

**Per-kind tallies, because the denominators are nothing alike.** *"37 of 37
templates, 26 of 30 fields, and 68 of 76 field values differ"* says something
that a single "131 of 143" does not, and the reader needs to know which
denominator the answer at the top of the list came from.

**Comparing against an empty window is answered in words.** With nothing on one
side there is no share to compare against and every item scores zero, so the
report says *"everything in the after window is new"* and offers the filter that
lists it, rather than printing a thousand rows all equally unsurprising.

**Two defects found on the way**, both pre-existing and both filed rather than
fixed here: EC022, a control character in a field name breaking any query that
references it, and EC023, a negated filter being unread as a flag before the
command sees it. Neither is a comparison bug; a comparison is just the first
thing that references every field at once, and writing its examples is the first
thing that tried to type a negation on a command line.

**Watch:** the next request will be a `--only patterns` flag, and it should
probably stay no. The ranking is what makes one list better than three, and a
flag that narrows it is a flag that hides the finding when it turns out to be a
field value.

---

## EC008 — Broaden intake

**Status: done.** Both halves complete and tested. Work is on branch `EC008`,
cut from `main`. Parsers are the contribution surface, so this doubles as
community fuel.

### EC008.1 — Compression — **done**

- [x] zstd — licence and binary-size cost checked before asking; approved
- [x] bzip2 and xz (stdlib has bzip2; xz does not)
- [x] Each removed from the skip list only once it actually reads
- [x] Compressed files are not `Tailable` — EC001 already assumes this, and it
      is now asserted per codec
- [x] Detection from magic bytes, for files and for pipes alike

**The dependency numbers the item asked for.** Both are BSD-3-Clause, neither
has a transitive dependency, and together they cost **+0.51MB on a 54MB binary
(+0.9%)**. Measured in isolation the pair looked like +1.07MB; on the real
binary it is half that, because `klauspost/compress` was *already in the build
graph* — go-duckdb pulls it in through arrow, and the shipped binary already
linked nine of its zstd symbols. Only the decoder itself is new code.

It was pinned rather than upgraded. `go get` wanted to move the shared module
from v1.17.11 to v1.19.2, which would have changed a dependency go-duckdb and
arrow both rely on, to gain nothing: zstd decoding has been stable for years.
`go mod tidy` keeps it where arrow put it.

**Magic bytes, never the extension.** A `.log` that is really gzip is common,
logrotate's `.1.gz` and `.1.zst` sit in the same directory as files with no
suffix at all, and the walker already refused to trust names for gzip. The four
signatures are checked against the first six bytes — xz's is the longest — and
a pipe is peeked for the same six, so `zstdcat old.log.zst | loupe` needs no
flag.

**bzip2 needs its level digit.** The signature is `BZh` followed by `1`-`9`.
Without the digit check, a text file beginning "BZh" — a hostname, a hash —
would be handed to a decompressor that then fails the whole file over three
bytes.

**zstd is pinned to one decoder goroutine per file.** The ingest already reads
sources in parallel; a decoder that spawns a worker per core per file turns a
directory of two hundred rotated archives into a thread explosion.

**The compression extensions came off the skip list; the container formats
stayed on.** `.zip`, `.tar` and `.7z` hold many files, and reading one as a byte
stream produces nonsense. `.gz`, `.zst`, `.bz2` and `.xz` hold exactly one, which
is the distinction that matters. Leaving them skipped meant a directory of
rotated logs read only the live file — silently, which is the failure this
project refuses.

**One suffix list, shared — which is what caught a real bug.** `internal/store`
had its own copy of "reduce a path to a source name", and it knew only `.gz`.
The moment the walker started returning `access.log.2.zst`, that archive became
a source of its own and `source:access` stopped matching it. Both now call
`source.TrimCompressionSuffix`, and a test pins every codec. The bug predates
this item — it was simply unreachable while the files were skipped.

### EC008.2 — Formats — **done**

- [x] journald JSON export (`journalctl -o json`)
- [x] Docker `json-file`
- [x] CRI / Kubernetes container log format
- [x] Golden fixture each, messy per `CLAUDE.md`: blank lines, a truncated final
      line, mixed timestamp forms, at least one malformed record
- [x] None of the nine formats claims another's lines

**journald is its own parser, not extra key names in jsonl.** Almost nothing
about a journal entry follows the conventions the generic JSON parser assumes:
the timestamp is microseconds since the epoch *in a string* — quoted because the
value needs 51 bits and a JSON double cannot hold it exactly — the level is a
syslog priority digit, and `MESSAGE` arrives as an array of byte values whenever
it holds text JSON cannot carry. Teaching jsonl those three exceptions would
make every other JSON log pay for them.

The priority goes through `severityLevel`, the function the syslog parser
already uses, because it is the same scale. A second mapping here could disagree
with that one. The digit is kept as a field beside the word: it is what a
systemd user filters on, and translating it away would lose the form they know.

**Docker's driver appends the newline the process wrote.** jsonl would half-read
a json-file line — `time` and `log` are both in its key lists — and leave that
newline on every message, putting a blank line under every record in the table
and a stray `\n` in every handoff. The half it gets wrong is the half that
matters.

The trio `log`, `stream`, `time` is the signature, and all three are required.
Two of them are ordinary names any application log might use.

**Neither container format carries a severity, so it is read out of the message
text.** Guessing from the stream would be worse than not guessing: plenty of
programs write ordinary progress to stderr, and marking all of it error-level
would poison `level:>=error` for the whole source. Reading the word the program
actually wrote is the same rule the fallback parser has always used, now shared
as `levelFromMessage`.

**CRI partial lines are marked, not stitched.** A `P`-tagged line is a fragment
the runtime split at its buffer. Rejoining them needs to know that the
*previous* line was tagged `P`, and `Continuer.IsContinuation` asks the opposite
question — whether this line continues the last one — so making it fit would
mean widening the contribution surface for one format, which `CLAUDE.md`
forbids. Nothing is lost: `partial:true` finds every fragment and they sit
adjacent on the timeline. Worth revisiting only if someone asks.

**The generic JSON parser now claims at most 0.85.** Three JSON formats
competing was new: jsonl returned 1.0 for anything that parses, so a journald
export and a Docker line both tied with it and were separated by alphabetical
order — an accident waiting to be renamed. A parser's confidence says how
*specifically* it claims the format, which is the reasoning `fallbackParser`
already uses at the other end of the scale at 0.01. The ceiling is 0.85 rather
than 0.9 because `Detection.Ambiguous` is a tenth: at 0.9 the specific parser
wins by exactly 0.1 and is reported as a coin flip anyway, which is the opposite
of the point.

**The existing suite caught both of the mistakes in this half.**
`TestFixturesAreMessy` refused fixtures under twenty records, and
`TestSpecificJSONParsersBeatGenericJSON` — written for this item — is what
surfaced the 0.9 boundary. Neither would have been noticed by reading the code.

**A gap found writing the examples, filed as EC024.** `/var/log/containers` is
the directory Kubernetes documentation points a log tool at, and every entry in
it is a symlink into `/var/log/pods`. The walk reads regular files only, so it
skips the lot — with a reason per file, which is the one thing it gets right.
CRI works against `/var/log/pods`, which holds the real files, and `README.md`
says so; the symlink case needs deduplication by resolved path before it can be
turned on, or pointing at `/var/log` would read every pod log twice.

**Watch:** three formats is where a `--parser` list stops being readable.
`loupe sources` already reports what was detected per file, and that is the
place to look when a format is guessed wrong, not a longer help string.

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

## EC012 — Fuzz the parsers and the DSL lexer

**Status: done.** Tier 1. Work is on branch `EC012`, cut from `main`
after EC005 merged.

The roadmap carried a row for this and no body, so the checklist below is
written from the contracts the code already states rather than from a spec.

Every parser is fed bytes chosen by somebody else. `CLAUDE.md` promises that a
malformed line never aborts a file and that no key is ever dropped, and the
`Parser` interface is the contribution surface — a parser arriving by PR from a
stranger is exactly the code most worth fuzzing. Go has native fuzzing in the
standard library, so this adds no dependency.

- [x] Every registered parser: no panic on any input, on any byte sequence
- [x] `Parse` must not mutate the line it was given — the reader keeps its own
      copy as `raw`, and a parser writing into that buffer would corrupt it
- [x] `Detect` returns a confidence inside 0.0–1.0, as its doc comment promises
- [x] A returned timestamp always carries a location, or every later time
      comparison panics
- [x] `parse.ReadAll`: no panic, and the stats reconcile against the lines read
- [x] The DSL: `query.Parse` never panics, and `parse(render(ast)) == ast` for
      anything that parsed
- [x] `pattern.Of`: no panic, never emits a control character, and the same
      input always yields the same id
- [x] Seed corpora drawn from the checked-in fixtures, so the corpus starts on
      real formats rather than random noise
- [x] Findings, if any, fixed rather than papered over with a seed exclusion

**Ten targets**, in three files:

| File | Targets |
| --- | --- |
| `internal/parse/fuzz_test.go` | `FuzzParsers`, `FuzzParseIsDeterministic`, `FuzzDetect`, `FuzzReadAll` |
| `internal/query/fuzz_test.go` | `FuzzParse`, `FuzzCompile`, `FuzzParseDuration` |
| `internal/pattern/fuzz_test.go` | `FuzzOf`, `FuzzOfIsStable`, `FuzzTemplateLeavesProseAlone` |

`FuzzCompile` checks the thing `CLAUDE.md` forbids getting wrong: one bound
argument per `?`, over every shape of input rather than the handful a table test
can list.

### What it found

**The parsers came through clean.** Nothing panicked, nothing mutated its input,
no confidence escaped 0.0–1.0, and `ReadAll`'s stats reconciled on every input.
Every finding was in the filter DSL, and all but two were in the same place: the
renderer disagreeing with the lexer about what a token is. That matters more
than it sounds, because the UI writes rendered ASTs back into the filter box —
a filter that does not survive render-and-reparse is one the user cannot re-run
or share, and it changes under them without saying so.

| Finding | Fix |
| --- | --- |
| `last:99999999999999999999d` overflowed `int64` and wrapped to a window ending before it started | refuse anything past ~292 years, and say why |
| `last:nans` passed both guards — NaN is neither `< 0` nor `> MaxInt64` — and returned the most negative duration there is | reject NaN and infinity by name |
| A search term containing invalid UTF-8 rendered as U+FFFD, silently becoming a search for something else | `quote` copies bytes instead of ranging over runes |
| `"" -` was an error but `""-` produced a term whose text was a lone minus, which then would not parse | a closing quote ends a term, as whitespace does |
| `"\r":0` rendered as `\r:0` — `needsQuoting` carried its own list of space characters and had missed carriage return | ask `unicode.IsSpace`, which is what the lexer asks |
| `A:=>` means equals the literal `>`, but rendered as `A:>` — a comparison with no value. Same for `A:=~foo` | quote a value that leads with an operator character |
| `last:""` parsed into a term holding nothing, rendered as `last:`, and read back as `last:` plus whatever word followed | a blank time is a missing time, with the same message |
| `after:"14:00 x"` rendered bare and read back as only its first half; `on:":"` rendered as `on::` | quote a time expression that would not lex back as one token |
| `0:" 0"` rendered as the phrase `"0: 0"` — a bare range has no keyword to sit outside the quotes | a bare clock range takes only bare words; anything else is a field |

Each has a named regression test alongside the fuzz corpus entry, so the reason
survives even if the corpus is regenerated.

Two of the properties turned out to be wrong rather than the code. A tab is not
damage — it is a legitimate separator, which is why `isControl` excludes it. And
an idempotency target on `Template` was removed: it found a real asymmetry, but
`Template` has one call site and is fed raw message text, never its own output,
so satisfying it would have meant changing what masks — which changes every
template id, and template ids are what `--new-since` compares against an earlier
run. The reasoning is recorded where the target used to be.

**Cost note.** A Go fuzz target runs its seed corpus as an ordinary unit test
under `go test`, so CI gains the regression value with no workflow change and
no added runtime. Long fuzzing runs stay a manual, local activity: each target
was run for 45s here, and `FuzzParse` for a further five minutes (17.7M execs)
after the last fix.

---

## EC016 — `loupe fields` field discovery

**Status: done.** Work is on branch `EC016`, cut from `main`. This item was a
table row with no checklist, so the scope below was derived from the gap it
names; the shape is `loupe top`'s and `loupe sources`'.

Today a field is discovered by getting one wrong. Typo a name and the error
comes back with the list attached — which works, and is the wrong way round.
`loupe fields` answers the question directly, before the mistake.

- [x] `loupe fields [directory] [filter]`, aliased `schema`
- [x] Every name a filter can use: built-in columns, promoted fields, and keys
      still in the JSON bag
- [x] Coverage, distinct count, type, and example values per field
- [x] A filter narrows the question — what do the *failing* records carry?
- [x] `--limit`, `--all`, and a stated count of what was cut
- [x] Fields no matching record carries are counted, not listed
- [x] The partly-numeric warning
- [x] `README.md` and `docs/FILTER-DSL.md` §7

```
FIELD       RECORDS  COVERAGE  DISTINCT  TYPE       STORED  EXAMPLES
level        33,671     99.2%  5         string     column  info, error, debug
path         26,115     76.9%  12        string     column  /healthz, /api/checkout, /api/cart
trace_id     18,742     55.2%  12,912    string     column  f77a05eb, b7217303, ceb5650f
latency_ms   12,812     37.8%  1,460     integer    column  201, 42, 182
enabled         424      1.2%  2         boolean    bag     false, true
```

**The columns were chosen by what changes the next command.** DISTINCT is the
one that earns its place hardest: three values is a distribution worth running
`loupe top` on, twelve thousand is an identifier and the answer is
`trace_id:f77a05eb`. Without it a reader has to run `top` to find out that
running `top` was the wrong idea.

**STORED is in the table rather than the footer** because it is the answer to
"why is this filter slower than that one", and somebody asking that is already
looking at this table. Two values, not three: a built-in column and a promoted
one behave identically, and the only distinction that changes anything is
column against bag.

**The warning this command exists to give.** A field that is a number on most
records and text on the rest — `latency_ms` holding `9000` and `"timed out"` —
loses the text ones to `latency_ms:>1000` without a word, because an ordering
comparison casts and a value that will not cast is skipped. The listing counts
both and says so:

```
4 of 5 values are numbers, so latency_ms:>N skips the other 1.
```

It fires on a *majority* rather than on any, which is the difference between a
warning and noise: three log messages that happen to be bare numbers do not make
`message` a numeric field, and a note on every text column would bury the real
one. The count comes from the same `TRY_CAST` the comparison itself makes, so it
is not an estimate of the behaviour — it is the behaviour.

**A field holding more than one JSON type is named too**, with the list. That is
the same hazard one level down, and it only arises for bag fields: a promoted
field has one type by construction, because promotion already resolved the
mixture by choosing `VARCHAR` — which is exactly when the numeric warning above
takes over. The two cover the same problem on either side of the promotion
threshold.

**A field no matching record carries is counted, not listed.** Its row would be
a line of zeroes, and thirty of those bury the fields the question was about.
The count is the thing that separates *"missing from my results"* from
*"missing from the data"*, which is the confusion a filtered listing invites.

**One statement for the whole listing.** Every field's count, distinct count,
numeric count and examples are aggregates in a single pass, because this is the
command someone runs on a directory nobody has looked at yet and thirty separate
scans of a 10M-row table would make the answer not worth waiting for. Examples
come from `approx_top_k`, which is approximate on purpose: these are examples of
what a value looks like, not a ranking, and an exact top-k would mean a `GROUP
BY` per field. The one thing that gets a second query is the type list for a
mixed field, and only when the first pass found one.

**Aliases are not listed twice.** `Schema.Known` — the list the unknown-field
error prints — includes `msg`, `line` and `pattern` beside `message`, `line_no`
and `pattern_id`. A table with both `message` and `msg` in it answers a question
nobody asked, so the built-in set is written out and the footer says the aliases
still work.

**Ordered by coverage, best covered first**, which is the order `loupe top` and
`loupe patterns` both use. Alphabetical would be better for looking one name up
and worse for the question actually being asked, which is "what is here".

**Watch:** the next request will be `--json` for scripting. `FieldSet` is
already tagged for it and the shape is stable, but the flag should wait for
someone who wants it — `loupe sql` can already produce anything a script needs,
and this command's value is that a person reads it.

---

## EC022 — A control character in a field name breaks the query

**Status: not started.** Found while building EC007, which is the first thing
that references every field in one statement and so the first thing to meet it
on ordinary data — `cmd/blaster` generates a field called `iss\x00\x00uer`, and
`demo/` contains one.

`internal/query.jsonPath` builds `fields->>'$."<key>"'`, and
`escapeJSONPathKey` escapes backslash, double quote and single quote. It does
not escape control characters, and a NUL terminates the statement at the
database's C boundary, so DuckDB sees a string that never closes:

```
Parser Error: unterminated quoted string at or near "'$."iss
```

- [ ] Any query referencing such a field fails — a filter on it, `loupe top` on
      it, and `loupe diff`, which references all of them
- [ ] **Ingest fails outright** when the field is common enough for
      `internal/schema` to promote it to a column: `Open: promote fields: exec:
      Parser Error`. A two-record fixture with the field on both records
      reproduces it, which means a real log carrying one on most lines cannot be
      opened at all
- [ ] Decide the fix: escape the key into the path as `\uXXXX` if DuckDB's path
      parser accepts it, build the path by concatenation (`'$."' || chr(0) || …`)
      so no NUL appears in the statement text, or refuse to promote such a field
      and resolve it another way
- [ ] Whatever the fix, the field must stay *queryable*, not merely
      non-fatal — CLAUDE.md's rule is that a record is never silently dropped,
      and a field nothing can reference is the same failure one level down
- [ ] Fuzz the key path: `FuzzParse` covers the DSL text, nothing covers a field
      name arriving from a log file into a JSON path literal
- [ ] EC007 works around it by leaving such fields out of a comparison and
      naming them; remove the workaround in `internal/session/diff.go`
      (`referenceable`) once the underlying escape is fixed

---

## EC023 — A negated filter is unusable without `--`

**Status: not started.** Found while writing the EC007 examples in `README.md`.
Affects every command that takes a filter positionally, not just `loupe diff`.

Negation is a headline feature of the filter language — `README.md` advertises
`-source:nginx` in the syntax list — but cobra reads a leading `-` as a flag
before the command ever sees it:

```
$ loupe ./logs '-source:nginx'
loupe: unknown shorthand flag: 's' in -source:nginx

$ loupe top level ./logs '-source:nginx'
loupe: unknown shorthand flag: 's' in -source:nginx
```

The escape hatch works and nothing says so:

```
$ loupe ./logs -- '-source:nginx'
$ loupe diff ./logs --before 13:00-14:00 --after 14:00-15:00 -- '-source:nginx'
```

- [ ] Decide the fix. `Flags().SetInterspersed(false)` stops flag parsing at the
      first positional, which fixes negation but breaks `loupe ./logs
      'level:error' --limit 5` — flags after the filter are the common shape, so
      that trade is probably wrong
- [ ] More likely: catch the error and rewrite it. cobra's message names the
      offending argument, and turning *unknown shorthand flag: 's' in
      -source:nginx* into *`-source:nginx` looks like a flag; write it after `--`*
      costs nothing and is the only thing the user needs
- [ ] Whichever way, `README.md`'s filter syntax list should show the `--` form
      beside the negation examples, since that list is where people copy from
- [ ] Applies to the default command, `top`, `patterns`, `histogram`, `trace`
      and `diff` — anything routed through `resolveArgs`
- [ ] EC019 (`cmd/loupe` test coverage) is where a regression test for this
      belongs; there is currently no test that runs a negated filter through
      argument parsing

---

## EC024 — Symlinked log files are skipped

**Status: not started.** Found while writing the EC008 examples in `README.md`.

`/var/log/containers` is the directory people are told to point a log tool at on
a Kubernetes node — it is what most shipper documentation names — and every
entry in it is a symlink into `/var/log/pods`. The walk reads regular files only,
so the whole directory is skipped:

```
$ loupe /var/log/containers
loupe: no readable log files in /var/log/containers, but 42 file(s) were skipped:
  /var/log/containers/checkout-abc123.log: not a regular file
```

The skip is reported rather than silent, which is the one thing this gets right.
EC008 shipped a CRI parser that cannot be pointed at the canonical CRI
directory, so the format is only half delivered.

- [ ] Follow a symlink that resolves to a regular file. `filepath.WalkDir` does
      not descend into symlinked *directories*, so only file symlinks are in
      play and there is no loop to guard against
- [ ] **Deduplicate by resolved path**, which is the part that needs care:
      `loupe /var/log` would otherwise read every pod log twice, once under
      `pods/` and once under `containers/`, and silently double every count.
      `Fingerprint` is path-based, so it will not catch this on its own
- [ ] Decide what a broken symlink is — a skip with a reason, almost certainly,
      not an error
- [ ] Keep skipping sockets, devices and FIFOs: a symlink to a regular file is a
      regular file for reading, and nothing else is
- [ ] Test that pointing at a tree containing both a file and a symlink to it
      produces each record once, and that the count is stated
- [ ] `README.md` currently steers people to `/var/log/pods` and explains why;
      remove that note once this lands

---

## Notes

- Items are numbered `ECnnn` and get a branch of the same name. Sub-tasks are
  `ECnnn.n` for anything worth its own commit.
- Update the status table at the top when an item moves. It is the part people
  actually read.
- Three Tier 3 entries were reconstructed from a partially garbled source; the
  intent is captured but check EC009–EC011 against what you meant.
