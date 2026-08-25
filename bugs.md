# loupe — bugs and usability findings

From a single 24m26s session against `platform-mixed.log` (250,000 lines, 48.1 MB, 15 formats
in one file). Binary built from `LOUPE` HEAD `3bbc396`. Every item below was reproduced at
least twice; the commands to reproduce are inline.

Ranked by how much damage they do to someone using this at 4am.

---

## Status

Worked through on branch `Bugs/First-take`. Full write-up, with before/after
output and screenshots, in `docs/verification/BUGS-VERIFICATION.md`.

- [x] **0.** No Go toolchain — installed Go 1.24.6 to `~/sdk/go1.24.6`
- [x] **1.** One malformed line aborts the entire ingest
- [x] **2.** A failed ingest is cached and silently reused
- [x] **3.** One file gets one parser
- [x] **4.** Any all-lowercase search term hangs forever — *root cause fixed; the
      hang itself did not reproduce here, see the note on that item*
- [x] **5.** `loupe sql` silently shifts TIMESTAMP columns
- [x] **6.** `loupe trace` picks the correlation field by coverage
- [x] **7.** `loupe top` cannot break down a value inside unparsed text
- [x] **8.** No way to see the lines around a line
- **9.** Smaller things — six of seven:
  - [x] `--format table` not the default off a TTY, undocumented
  - [x] `--format raw` ignores the columns you selected
  - [x] An unknown `--parser` warns instead of erroring
  - [ ] **No ICU extension, so `AT TIME ZONE` fails** — not fixable without an
        outbound request, which invariant 4 forbids. The error now explains
        itself and names the alternatives; see the note on that item.
  - [x] "the original read took 6.001s" is stale
  - [x] No fold/dedup in the record listing
  - [x] `loupe cache --clear` is not a thing

Verified by 596 Go tests (47 new) and 44 Playwright tests (10 new), with
`go vet`, `gofmt` and `golangci-lint` clean.

---

## 0. Blocker: the provided source cannot be built on this machine

> **Fixed.** Go 1.24.6 installed to `~/sdk/go1.24.6`; `gcc` was already present for CGO.
> Nothing in the repo changed for this.

Not a loupe defect, but it stopped the benchmark before it started, so it belongs here.

```bash
cd LOUPE && make build
# go: command not found
```

There is no Go toolchain installed (`/usr/local/go` absent, `~/go/bin` contains only
`golangci-lint`), and loupe requires CGO to link DuckDB, so `go install` would not have helped
either. `LOUPE/` ships source with no binary and no vendored release archive.

I proceeded with a pre-existing binary found at
`/tmp/claude-1000/-home-grantseymour-WebstormProjects-laupe/.../loupe-main`, built at 18:29 —
the same minute `LOUPE/` was placed, from the same HEAD commit. **This is a caveat on every
result in `commands.md`:** I could not verify the binary was built from the exact tree provided.
For future runs, ship a built binary alongside the source, or install Go.

---

## 1. One malformed line aborts the entire ingest

> **Fixed.** Records are sanitised at the appender boundary (`internal/store/utf8.go`):
> invalid sequences become U+FFFD, the original bytes are kept hex-encoded in the
> fields bag under `loupe_raw_hex`, and the count is stated on the status line.
> Your suggested fix, taken as written.

**Severity: critical.** Violates the project's own first invariant, stated in `CLAUDE.md`:
*"A malformed line never aborts a file. Keep the raw text, mark it unparsed."*

Very first command of the session, zero config, exactly as `README.md` advertises:

```bash
$ loupe platform-mixed.log --limit 5
loupe: promote fields: exec: Invalid Input Error: Invalid unicode (byte sequence mismatch) detected in segment statistics update
```

No records, no partial results, no indication of which line is at fault or how to skip it. On
the default (detected `jsonl`) parser this is **intermittent** — it fired on the first run and
not on subsequent fresh-cache runs. But forcing the best-effort parser, which is exactly what
the README tells you to reach for with mixed formats, fails **every single time**:

```bash
$ loupe platform-mixed.log --parser text --limit 1 --no-cache
loupe: close appender: database/sql/driver: duckdb error: Invalid unicode (byte sequence mismatch) detected in segment statistics update
duckdb error: Invalid unicode (byte sequence mismatch) detected in segment statistics update
could not close appender: appended data has been invalidated due to corrupt row
```

Reproduced 2/2. The culprit is **line 140,469**, a logfmt record from `search-svc` carrying
invalid UTF-8 bytes in a `raw=` field — 1 line out of 250,000, i.e. 0.0004% of the input, takes
down 100% of it.

**Impact.** The whole point of this tool is reading logs that are already a mess. Binary
garbage in a log line is not an exotic edge case; it is Tuesday. Note also that the same
corpus contains a line whose *content* is `UnicodeDecodeError: 'utf-8' codec can't decode byte
0x9c…` — the systems being logged already hit this, so the log tool must not.

**Suggested fix.** Sanitise on ingest at the appender boundary: replace invalid sequences with
U+FFFD, keep the original bytes in a side column or as hex, and report `N line(s) contained
invalid UTF-8 and were stored with replacement characters` in the status line. That satisfies
"never lose data silently" in both directions.

---

## 2. A failed ingest is cached, and later runs silently serve the degraded result

> **Fixed.** The cache is stamped complete only once the whole pipeline — schema
> inference included — has succeeded (`store.DB.MarkComplete`), and an unstamped
> file is never reused. The invalid-UTF-8 count is carried in the cache summary
> too, so a cached run does not get quieter about its own caveats.

**Severity: critical.** This is the "confident wrong conclusions during incidents" failure mode
`CLAUDE.md` names explicitly.

After the crash in #1, the *next* run succeeded — from a cache entry the failed run had written:

```bash
$ loupe platform-mixed.log --limit 5
1 source(s) · 250000 records · 211274 unparsed · 211274 without a timestamp
Reused a cached ingest — the original read took 6.001s. Pass --no-cache to re-read the files.
```

Compare a clean run:

```bash
$ loupe platform-mixed.log --limit 1 --no-cache
1 source(s) · 250000 records · 211274 unparsed · 211274 without a timestamp
Promoted 8 field(s) to columns: service (VARCHAR), duration_ms (DOUBLE), env (VARCHAR), host (VARCHAR), request_id (VARCHAR), span_id (VARCHAR), and 2 more
```

The `Promoted 8 field(s)` line is **absent** from the cached run. The field-promotion step is
what the crash killed, so the cached ingest has **no promoted columns** — `service`, `host`,
`version`, `request_id`, `span_id`, `duration_ms`, `env`, `trace_id` are all missing. Every one
of those is a field you would filter on. Nothing in the output says the cached ingest is
incomplete; the record count is identical, so it looks healthy.

Had I not happened to run `--no-cache` while investigating something else, `service:payments-api`
would have errored as an unknown field and I would have concluded the data does not carry it.

**Suggested fix.** Write the cache entry only after the full ingest pipeline succeeds
(promotion included), or stamp it with a completion marker and refuse to reuse an unmarked one.
A cache must never be able to hold a partial result.

---

## 3. One file gets one parser, and the README's remedy does not exist for a merged stream

> **Fixed.** Per-line format detection, chosen automatically when the detected parser
> covers under 80% of the sampled lines, and available by hand as `--parser mixed`.
> Each record now carries the format that read it, so `format:nginx` works inside a
> merged file and `loupe sources` shows the real breakdown. Field promotion now
> stratifies by source *and* format, without which `loupe top` and `stats … by` would
> have stayed just as unusable. On a corpus of this shape: 80.7% unparsed → 0.6%.
> `loupe sources` also warns when over half a file is unparsed, as you suggested.

**Severity: high (design).**

```bash
$ loupe sources platform-mixed.log
FILE                FORMAT  RECORDS  UNPARSED  NO TIMESTAMP  RANGE                                      TIMEZONE
platform-mixed.log  jsonl   250000   211274    211274        2026-08-20 21:00:01 – 2026-08-21 07:34:31  known (carried in the format)
```

**211,274 of 250,000 records (84.5%) unparsed, and — the part that hurts — 84.5% with no
timestamp**, therefore not on the timeline at all. loupe's headline promise is "point it at logs
in mixed formats, get one timeline." On this corpus it delivers a timeline of 15.5% of the data
and a text search over the rest.

The README documents this honestly for **pipes** ("One pipe is one format", with a worked
example showing `status:>=500` returning 7,267 instead of 14,480) and the fix it offers is
"point loupe at the directory." That fix is unavailable here: **the corpus is a single merged
file on disk**, which is precisely what `journalctl`, a k8s log collector, or any
`cat *.log > combined.log` produces. Nothing in `README.md` or `docs/` warns that a *file* can
have the same problem.

The status line does say `211274 unparsed`, which is good and is why I caught it immediately.
But `--parser` forces exactly one format for the whole file, so there is no way to express
"this file contains many formats, detect per line." The one parser designed for heterogeneous
input (`text`) crashes deterministically — see #1.

**Impact, concretely.** Every Tier 2 counting task had to be done in `loupe sql` against the
`raw` text column, hand-writing regexes for nginx, haproxy and Postgres line shapes. `loupe top`
and `stats … by <field>` — the two commands built for exactly those questions — were unusable,
because the fields do not exist. That is the difference between a 30-second answer and a
5-minute one, repeated nine times.

**Suggested fix.** Per-line format detection as an opt-in (`--parser auto-per-line` /
`--mixed`). The parsers already exist and detection already works on content; the constraint is
that it is applied once per file rather than once per line. Failing that, `loupe sources` should
say something when the unparsed fraction is over ~50% — *"84.5% of this file did not match the
detected format `jsonl`; it may contain multiple formats"* — rather than reporting a number and
leaving the inference to the user.

---

## 4. Any all-lowercase search term hangs forever

> **Fixed.** Both mechanisms you identified are gone: no invalid UTF-8 reaches the store
> any more (item 1), and case-insensitive matching compiles to
> `regexp_matches(x, '(?i)…')` rather than `lower(x) LIKE …` — which benchmarked
> marginally *faster* over 1.9M rows, so it cost nothing.
>
> **One caveat, since it matters.** I could not reproduce the hang itself on this
> machine (DuckDB 1.1.3 via go-duckdb 1.8.5). I probed `lower()` directly with
> several invalid byte sequences, including a 2 KB densely-invalid string inserted
> via a BLOB cast to bypass the appender, and it returned instantly every time. The
> *ingest crash* from the same line reproduces on every run. So the hang may have
> depended on your exact bytes, a different DuckDB build, or the poisoned-cache
> state from item 2. I have fixed both things it was attributed to; I never saw the
> symptom.

**Severity: critical.** This is the worst usability bug in the tool, because lowercase is what
people type.

```bash
$ loupe platform-mixed.log 'req-7f3c9a2e-4d61-4b8f-9c02-aa15d3e77b10'
# no output; killed after 2 minutes
```

Two CPU cores pinned, no output, no progress indicator, no timeout. I let one run for 400
seconds before killing it; it never returned. The equivalent SQL returns in **0.11 s**.

The trigger is **smart case** (`docs/FILTER-DSL.md` §5: "case-insensitive unless the pattern
contains an uppercase character"). Adding one capital letter makes it instant:

```
TERM              TIME     RESULT
ZEBRAFISH-7742     0.27s    1
zebrafish-7742    20.03s    (timeout)
ZEBRAFISH          0.28s    1
zebrafish         20.01s    (timeout)
Failed             0.24s    2611
failed            20.03s    (timeout)
```

`message~failed` also hangs; `message~/failed/` (the regex form) returns in 0.20 s.

**Root cause — isolated exactly.** Case-insensitive matching compiles to `lower(raw) LIKE …`,
and `lower()` never returns on **line 140,469**, the invalid-UTF-8 line from bug #1:

```
PREDICATE                                              TIME     RESULT
raw LIKE '%rollback%'                                   0.11s    0
raw ILIKE '%rollback%'                                 12.01s    (timeout)
lower(raw) LIKE '%rollback%'                           12.01s    (timeout)
regexp_matches(raw,'(?i)rollback')                      0.11s    1
lower(raw) LIKE '%rollback%' AND line_no <> 140469      0.13s    1     ← excluding ONE line
line_no <> 140469 AND lower(raw) LIKE '%rollback%'      0.14s    1
```

```bash
$ loupe sql platform-mixed.log "SELECT length(lower(raw)) FROM logs WHERE line_no = 140469"
# hangs; killed at 12s
```

`lower()` on that single row alone never completes. So **one malformed line disables
case-insensitive search across the entire tool** — the filter box, `loupe patterns` filtering,
the UI, `/api/query`, everything that takes a bare word.

Note how bugs #1 and #4 compound: the same line either crashes your ingest or, if you get past
that, silently makes the primary interaction hang. And the symptom is maximally confusing —
searching `ZEBRAFISH` works instantly while searching `zebrafish` hangs forever, which looks
like nothing and reads like the tool is broken at random.

**Suggested fix.** Fixing #1 (sanitise invalid UTF-8 at ingest) fixes this too, and is the right
layer. Independently: compile case-insensitive matching to `regexp_matches(raw, '(?i)…')`, which
is fast **and** correct here, rather than `lower() LIKE`. Separately, no filter should be able to
run unbounded with no output — a query that exceeds a few seconds should print what it is doing.

**Workarounds** (both used in `commands.md`): include an uppercase character to trigger
case-sensitive matching, or use the regex form `message~/pattern/`.

---

## 5. `loupe sql` silently shifts TIMESTAMP columns by the display offset

> **Fixed.** Only `ts` and columns DuckDB types as `TIMESTAMP WITH TIME ZONE` are
> converted now. Anything else in `loupe sql` renders exactly as computed — and,
> since every conversion in this tool is announced, the *absence* of one is
> announced too: *"Shown exactly as computed, not converted to Australia/Brisbane:
> as_timestamp. Only ts is known to hold UTC."*

**Severity: high.** Produces wrong times with no warning, in the one command whose whole purpose
is answering questions the DSL cannot.

```bash
$ loupe sql platform-mixed.log "SELECT TIMESTAMP '2026-08-20 22:32:02' AS as_timestamp, (TIMESTAMP '2026-08-20 22:32:02')::VARCHAR AS as_varchar"
AS_TIMESTAMP             AS_VARCHAR
2026-08-21 08:32:02.000  2026-08-20 22:32:02
```

A **literal** timestamp comes back **10 hours later and on the wrong day**. The renderer treats
every TIMESTAMP-typed value as UTC and converts it into the display timezone
(Australia/Brisbane, +10:00) — including values the user computed themselves, which were never
UTC. With `--utc` the same query renders correctly, which confirms the conversion is the cause.

This cost me three wrong iterations of Task 24 and very nearly a wrong answer: I had normalised
nginx `+10:00` and app-JSON `Z` timestamps to a common basis, and the output showed both series
shifted by 10 hours in lockstep. Because *both* were shifted, the table looked internally
consistent and plausible. I only caught it by testing a literal.

This is the exact failure `docs/FILTER-DSL.md` §2.3 is written to prevent — *"This mismatch is
where log tools lose users' trust"* — appearing in the tool's own escape hatch.

**Suggested fix.** A TIMESTAMP from user SQL is a naive value and should be rendered verbatim.
Only apply display-timezone conversion to `TIMESTAMPTZ`, or to the known `ts` column. If the
conversion must stay, the status line has to say *"timestamp columns converted from UTC to
Australia/Brisbane"*, the way every other conversion in this tool is announced.

**Workaround:** cast to `::VARCHAR` in `loupe sql`, or pass `--utc`.

---

## 6. `loupe trace` picks the correlation field by coverage and gets it wrong

> **Fixed.** Detection prefers the field that actually contains the id being asked for,
> falling back to coverage only when that cannot decide. And a trace now includes
> the records that mention the id in their text without carrying it as a field —
> marked, counted, and captioned as the looser match it is. Your Task 18 now
> returns all six lines rather than one.

**Severity: medium.** Good failure message, wrong outcome.

```bash
$ loupe trace req-7f3c9a2e-4d61-4b8f-9c02-aa15d3e77b10 platform-mixed.log
No records carry trace_id req-7f3c9a2e-4d61-4b8f-9c02-aa15d3e77b10.
platform-mixed do record trace_id, so this id is not one of theirs.
```

The correlation field is chosen by "which one covers the most records", so `trace_id` (38,725
records) beat `correlation_id` (1 record). But the id the user pasted is *obviously* a
`correlation_id` — it is a value, and the tool knows which field holds it. Detection should
consider **which field contains the id being asked for**, and only fall back to coverage when
that is ambiguous. `--field correlation_id` works but requires knowing the answer first.

Even then it would have found **1 of the 6 lines**, because the other five are unparsed text
where the id is not a field (bug #3). `loupe trace` cannot match against `raw`.

To the tool's credit, the message is honest and non-empty, and it correctly distinguishes
"this source records trace ids and none match" from "this source cannot record them" — the
two-kinds-of-silence design in the README is real and it works.

**Suggested fix.** Prefer the field that actually contains the requested value. Additionally,
fall back to a `raw` substring match and report those hits as "found in unparsed text, fields
unavailable" — that alone would have answered Task 18 completely.

---

## 7. `loupe top` cannot break down a value inside unparsed text, and the DSL will happily undercount

> **Fixed.** `loupe top` accepts a regex and counts the first capture group. A bare
> `/re/` reads the raw line; `field~/re/` reads a named field, spelled the way the
> filter language already spells a regex. The pattern is a parameter, never
> concatenated into the SQL. Your sshd example now answers 248 rather than 144.

**Severity: medium.** Consequence of #3, but worth stating separately because of how it fails.

Task 8 asked how many `Failed password` lines were for `root`. The natural query:

```bash
$ loupe platform-mixed.log '"Failed password for root" stats count()'
COUNT()
140
```

**140 is wrong; the answer is 243.** sshd writes two shapes — `Failed password for root` and
`Failed password for invalid user root` — and the second is a 42% undercount that looks like a
clean, confident answer. Nothing in the output hints that the phrase is a prefix of a longer
form.

The command that exists to prevent this is `loupe top`, but the username is inside unparsed
text, so there is no field to break down. I had to write the group-by myself:

```bash
$ loupe sql platform-mixed.log "SELECT regexp_extract(raw,'Failed password for (invalid user )?(\S+)',0) shape, count(*) n FROM logs WHERE raw LIKE '%Failed password%' GROUP BY 1 ORDER BY n DESC"
```

**Suggested fix.** Mostly fixed by #3. Independently, `loupe top` could accept a regex capture
(`loupe top '/Failed password for (?:invalid user )?(\S+)/'`) so value breakdowns work on text
that no parser claimed. That single feature would have collapsed most of my SQL in Tier 2.

---

## 8. No way to see the lines around a line

> **Fixed.** `-C/--context N` shows N records either side of every match, from the same
> file, in one listing — with a `hit` column so a block of five lines says which one
> was found, and `line_no` so gaps are visible. Neighbours are found by ingest order
> rather than line number, because a record can span many physical lines.

**Severity: medium (missing feature).**

`line_no:140469` jumps to a line — which is good, and I only found it by reading `loupe fields`
output; it is not in the README. But there is no `--context`/`-C` equivalent, and for a corpus
built on **multi-line records** (Java stack traces with `Caused by:`, Python tracebacks,
Postgres `ERROR`/`DETAIL`/`HINT`/`STATEMENT` blocks) that is a real gap. A stack trace is
useless one frame at a time.

`loupe patterns` confirms the shape of the problem — 21,930 Java continuation lines and 15,552
Python traceback lines are each indexed as their own record and their own template.

**Suggested fix.** `-C/--context N` on record listings, and multi-line record grouping in the
parsers that need it (the Log4j and Postgres parsers already know where a block starts).

---

## 9. Smaller things

> **Six of seven fixed.** Marked individually below. The ICU one cannot be fixed
> without breaking a hard invariant; the reasoning is with it.

- [x] **`--format table` is not the default off a TTY.** Every README example shows tables; piped
  output is NDJSON. Defensible, but undocumented — I thought the table renderer was broken until
  I passed `--format table` on a hunch.
  → Stated in the flag's own help: *"default table on a terminal, ndjson when piped"*.
- [x] **`--format raw` ignores the columns you selected.** `SELECT line_no, raw FROM …` under
  `--format raw` prints only `raw`, dropping `line_no` silently. It should either honour the
  projection or say it is printing raw lines only.
  → It says so now: *"--format raw prints the original line only; line_no is not shown
  (use --format csv or ndjson to keep them)."*
- [x] **An unknown `--parser` warns instead of erroring.**
  ```
  $ loupe platform-mixed.log --parser nosuchparser --limit 1
  no sources · 0 records
  Warning: unknown parser "nosuchparser" (available: cri, docker, journald, jsonl, log4j, logfmt, nginx, postgres, syslog, text)
  ```
  The candidate list is exactly right, but this is a typo returning zero records — which is the
  behaviour `docs/FILTER-DSL.md` §7 forbids for field names. It should be an error.
  → It is an error now, raised once before anything is read, and it exits non-zero.
- [ ] **The embedded DuckDB has no ICU extension**, so `AT TIME ZONE` fails in `loupe sql`:
  ```
  Binder Error: No function matches the given name and argument types 'timezone(STRING_LITERAL, TIMESTAMP WITH TIME ZONE)'
  ```
  Combined with #5, any cross-timezone work in `loupe sql` means doing offset arithmetic by
  hand — the thing this project repeatedly says it exists to spare people.
  → **Not fixed, and I do not think it can be here.** ICU is not statically linked into
  the embedded DuckDB, and `INSTALL icu` fetches over the network — which invariant 4
  forbids, and which is most of why this tool can be trusted with production logs. The
  failure is at least legible now instead of a raw binder error: it names the cause, says
  loupe already converts `ts` for you, and points at `--tz`/`--utc`. Fixing #5 removes most
  of the reason to reach for `AT TIME ZONE` in the first place. A real fix means a DuckDB
  build with ICU statically linked — worth its own decision, not something to slip into a
  bug-fix branch.
- [x] **"the original read took 6.001s" is stale**, replayed verbatim from whichever run built the
  cache. Minor, but it reads as a live measurement.
  → Reworded: *"Reused a cached ingest — reading these files cost 1.2s when the cache was
  built."*
- [x] **No fold/dedup in the record listing.** `loupe patterns` finds the 20,000× repeated line and
  ranks it first, but listing records still prints all 20,000. The benchmark's stated purpose for
  that block is testing dedup and scroll performance; the CLI has no dedup.
  → `--fold` collapses consecutive runs of the same template into one row with a count.
  Only consecutive runs, so the count means "this happened N times in a row here" rather
  than "this shape occurs N times somewhere". The footer says *runs*, not *records*.
- [x] **`loupe cache --clear` is not a thing** (it is `loupe cache clear`). Correctly rejected with
  `unknown flag: --clear` — noting it only because I lost a minute to my own error and a
  did-you-mean would have saved it.
  → It offers the verb now: *"did you mean `loupe cache clear`?"*

---

## What worked well

Worth recording, since the report above is unavoidably negative.

- **`loupe patterns` is the standout.** It answered Task 23 unprompted, on the first run,
  ranked first. Its footer is the best-written output in the tool: it explains that 93,158 of
  the templates come from unparsed records "where a broken line is its own shape", tells you
  `parsed:true` excludes them, and states the `ts:none` count. That is a tool being honest about
  its own limits, and it is why I understood the corpus within three commands.
- **The status line is genuinely load-bearing.** `250000 records · 211274 unparsed · 211274
  without a timestamp` on every single invocation is what made bug #3 obvious in 10 seconds
  instead of an hour. Most tools would have shown me 38,726 records and let me believe it.
- **Timezone handling is correct where it applies.** `--utc` / default display conversion on
  parsed records is exactly right, and the `Times shown in Australia/Brisbane (AEST, UTC+10:00)`
  banner appears everywhere it should. The bug in #5 is in `loupe sql`'s renderer, not in the
  timezone model.
- **`loupe sql` is a real escape hatch, not a token one.** Full DuckDB over a sensible schema
  (`raw`, `line_no`, `parsed`, `pattern`, `seq` all present and indexed) is what let me finish
  all 24 tasks without ever touching grep. Window functions, `ROLLUP`, `USING SAMPLE`,
  `regexp_extract_all` — everything I reached for was there.
- **Error messages name the alternatives.** Unknown parser lists the parsers; `loupe trace`
  explains *why* nothing matched. Where loupe fails, it usually fails legibly.
- **Speed, once ingested, is excellent.** 48 MB ingests in ~6 s, and every non-pathological
  query in this session returned in 0.05–0.30 s, including full-table regex scans over 250,000
  raw lines.

---

## Scorecard

| Metric | Result |
|---|---|
| Wall clock, all 24 tasks | **24m 26s** |
| loupe invocations | **41** (excluding bug-characterisation repeats) |
| Fell back to grep / awk / head | **0** — never left loupe |
| Tasks answered via the filter DSL alone | **6** of 24 (T1, T3, T6, T8-total, T19-partial, T24-partial) |
| Tasks requiring `loupe sql` | **18** of 24 |
| Tasks where a purpose-built command failed and I routed around it | **3** (T2 `sources`, T8 `top`, T18 `trace`) |
| Hard crashes | **2** (ingest, reproducible with `--parser text`) |
| Hangs (>2 min, no output) | **1 class** — every lowercase search term |
| Silent wrong answers avoided only by luck | **2** (#2 poisoned cache, #5 timestamp shift) |

**The headline.** loupe never had to be abandoned — the escape hatch is good enough that I
finished everything inside the tool. But **75% of the tasks went through `loupe sql`**, which
means for this corpus loupe was mostly a fast indexed DuckDB front-end rather than a log
explorer. Almost all of that traces to one design decision (one parser per file, #3) and one
malformed line (#1 and #4, which are the same root cause).

Those two are worth fixing before anything else: per-line format detection would move most of
Tier 2 back into the DSL, and sanitising invalid UTF-8 at ingest would fix both the crash and
the search hang. Neither is a large change, and together they are the difference between the
tool the README describes and the tool I used today.

---

## Outcome

Both of the two you called out have been done, and they were the right two.

Per-line format detection moved this corpus from 80.7% unparsed to 0.6%, which
puts `loupe top` and `stats … by <field>` back in reach and takes most of Tier 2
out of `loupe sql`. Sanitising invalid UTF-8 at ingest fixed the crash, and
removed the only thing the search hang was ever attributed to — though the hang
itself did not reproduce here, and item 4 says so rather than claiming a scalp.

Neither was a large change, as you guessed. The one thing that was not in the
report and cost the most care: per-line detection quietly mislabelled every
multi-line format, because coverage was counting Java stack-trace continuation
lines as lines the parser had failed to read. That halved ingest throughput and
broke stack traces back into separate records, and the record counts looked
fine throughout. There is a regression test on it now.

Full write-up with before/after output and screenshots:
`docs/verification/BUGS-VERIFICATION.md`.
