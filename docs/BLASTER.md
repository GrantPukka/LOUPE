# blaster — the log generator

`cmd/blaster` writes realistic, deliberately messy, multi-format log files. It
exists so that testing and demoing loupe never depends on having production logs
to hand, and so that the fixtures the parsers are tested against contain the
kinds of damage that real log directories contain.

It is stdlib-only and CGO-free, so it cross-compiles anywhere and runs in CI.

```bash
make blaster                                   # build it
make fixtures                                  # regenerate testdata/mixed
make demo                                      # generate ./demo and open it

go run ./cmd/blaster -out ./demo -scenario incident
go run ./cmd/blaster -out ./testdata/mixed -seed 7 -duration 5m -malform 0.02
go run ./cmd/blaster -out ./demo -follow -rate 40
```

---

## 1. What it is for

The point is not volume. Generating ten million clean JSON lines proves nothing;
any parser handles those. The point is that the emitted files are

- **in six different formats**, so format detection has something to detect,
- **causally correlated with each other**, so cross-source timeline correlation
  can be demonstrated and tested, and
- **broken in specific, realistic ways**, because clean synthetic fixtures hide
  exactly the bugs that matter.

That last property is the one that earns the tool its place in the repository.
`docs/FILTER-DSL.md` and `CLAUDE.md` both make never-silently-losing-data the
core correctness principle, and a fixture set with no damaged records cannot
test it.

---

## 2. Flags

| Flag | Default | Meaning |
|---|---|---|
| `-out` | `./demo` | Output directory, created if missing |
| `-scenario` | `incident` | `steady`, `incident`, `deploy-regression`, `quiet` |
| `-duration` | `18m` | Span of simulated time |
| `-rate` | `12` | Baseline requests per simulated second |
| `-seed` | `42` | RNG seed — the same seed gives byte-identical output |
| `-malform` | `0.015` | Fraction of lines that are broken on purpose |
| `-follow` | `false` | Write in real time (60× compressed) instead of all at once |
| `-rotate` | `true` | Also emit `access.log.1` and `access.log.2.gz` |

Simulated time always ends at `2026-08-13T14:20:00Z` and runs backwards by
`-duration`, so fixtures do not age and golden files stay stable.

**`-seed` determinism is a guarantee, not a coincidence.** Regenerating fixtures
with the same seed must produce identical bytes, or golden-file tests become
noise. There is a test for this.

---

## 3. What it emits

Six logical sources, one per format:

| File | Format | Timezone in format? |
|---|---|---|
| `checkout-api.log` | JSON lines | known — RFC3339 with offset |
| `auth-svc.log` | logfmt | known — RFC3339 with offset |
| `access.log` | nginx combined | known — `+0000` in the bracketed date |
| `payment-worker.log` | Log4j, with Java stack traces | **none — assumed** |
| `postgresql.log` | Postgres server log | abbreviation only (`UTC`) |
| `syslog` | syslog RFC5424 | known — RFC3339 |

Plus `access.log.1` and `access.log.2.gz` for rotation and transparent
decompression, and `manifest.json`.

The two sources with no reliable offset are there on purpose: they are the
`docs/FILTER-DSL.md` §2.5 trap, where a server on UTC and an operator on BST
silently disagree by an hour. Any change to timezone handling should be checked
against `payment-worker.log`.

### manifest.json

Records the scenario, the seed, and per file the format, line count, logical
record count, and how many lines were broken deliberately. Tests assert against
it, so a parser that quietly drops records fails rather than passing with a
smaller number.

Note that `lines` and `records` differ where a record spans lines — the Log4j
stack traces. A parser that reports one record per physical line is wrong, and
the manifest is what catches it.

---

## 4. The incident scenario

The default scenario is a cascading failure with a findable root cause. The
incident window sits at 60–72% through the run, leaving calm on both sides so a
demo can establish a baseline first.

Six seconds *before* any symptom appears:

```
postgres        WARN   connection pool exhausted, 100 of 100 connections in use
postgres        ERROR  FATAL: remaining connection slots are reserved for superusers
host            WARN   memory cgroup near limit: 3.8G of 4.0G
payment-worker  WARN   HikariPool-1 - Connection is not available, request timed out
```

Then the symptoms: `checkout-api` starts returning 502s with multi-second
latencies, `payment-worker` throws `PaymentGatewayException: read timed out`
with a full Java stack trace, and nginx logs the 502s.

Finding the Postgres warning by dragging the timeline back from the visible
error spike is the product demo. It is also the correctness test that matters
most: those four root-cause lines are in four different files in three different
formats, and they only line up if timestamp normalisation is right.

Requests fan out across services sharing a `trace_id`, so
`loupe ./demo 'trace_id:a91c40f2'` reconstructs one request's path across every
source.

---

## 5. How lines get broken

`-malform` controls the fraction. Five kinds of damage, chosen to match what
actually appears in log directories:

1. **Truncated mid-line** — the process was killed while writing.
2. **Interleaved write** — two threads writing to one descriptor, leaving NUL
   bytes in the middle of a line.
3. **An unescaped newline inside a JSON string value** — one logical record
   split across two physical lines, and invalid JSON on both.
4. **A blank line.**
5. **A line from an entirely unrelated format** leaking into the file, e.g. an
   nginx notice inside a JSON log.

Required behaviour for every one of these: the record is kept with its raw text
and marked unparsed, the file keeps being read, and the count is reported. A
parser that returns a fatal error for any of them is broken, and a run that
silently ends up with fewer records than `manifest.json` says is a bug.

Case 5 also tests detection: one nginx line inside 13,000 JSON lines must not
persuade the detector that the file is nginx.

---

## 6. Adding to it

Keep it stdlib-only — it must stay CGO-free so CI can run it on any runner.

When you add a parser, add its format here too. A parser whose only fixture is
hand-written tends to be tested against exactly the shape its author imagined.
Add a formatter function, add the sink to `sinks`, and make sure at least one
generated line for it is malformed.

If you add a format whose timestamps carry no offset, say so in the table in
section 3. That table is what someone reads when a timezone test starts failing.
