<h1 align="center">loupe</h1>
<p align="center">
  <b>Point it at a directory of logs. Get a searchable timeline in one second.</b><br>
  No Elasticsearch. No daemon. No Docker. One binary.
</p>

<p align="center">
  <a href="https://github.com/GrantPukka/loupe/actions/workflows/ci.yml"><img src="https://github.com/GrantPukka/loupe/actions/workflows/ci.yml/badge.svg" alt="ci"></a>
  <img src="https://img.shields.io/badge/license-MIT-blue" alt="MIT">
  <img src="https://img.shields.io/badge/go-1.24+-00ADD8" alt="Go 1.24+">
</p>

<!-- TODO: the ten-second GIF goes here, and it matters more than anything below it.
     Script: wide view → red error cluster is visible → drag the timeline to it →
     click a row → click status:502 → done. Record at 1200px, keep it under 3MB.
     Until it exists there is no <img> tag, because a broken image at the top of
     the page is worse than no image at all. -->

---

```bash
loupe ./logs                 # every log file in the directory, one timeline
loupe ./logs ~/Downloads     # several locations, still one timeline
loupe ./logs --ui            # same thing, in the browser
loupe tui ./logs             # same thing, in the terminal
loupe ./logs --follow        # keep watching as new records are written
loupe tui ./logs --follow    # the same, in the full-screen terminal view

loupe patterns ./logs        # 34,000 lines as a dozen message templates
loupe patterns ./logs --new-since 15m   # which shapes just started happening

loupe trace a91c40f2 ./logs  # one request across every service, with the waits

kubectl logs -f api | loupe  # read a pipe, live
loupe ./logs -               # a directory and a pipe, on one timeline

loupe subscribe /var/log     # remember it
loupe                        # read everything subscribed
```

## Why

Your log directory has Nginx access logs, a Java service writing Log4j with stack
traces, Postgres output, and one service that got the structured-logging memo.
Looking at them together currently means either standing up a logging stack or
opening four terminal windows.

loupe reads them all, normalises them to the same shape, and puts them on one
timeline — so you can watch a database connection pool exhaust itself, then the
app start erroring, then Nginx start returning 502s, in the order it actually
happened.

## Install

```bash
brew install GrantPukka/tap/loupe
```

Or take the archive for your platform from the [latest
release](https://github.com/GrantPukka/loupe/releases/latest) — Linux and macOS,
amd64 and arm64 — verify it against `checksums.txt`, and put `loupe` on your
PATH. Those binaries are unsigned, so macOS quarantines a downloaded one:
`xattr -d com.apple.quarantine ./loupe`, or use Homebrew, which sidesteps it.

From source needs Go 1.24+ and a C toolchain, because loupe links DuckDB
(`build-essential` on Linux, the Xcode command line tools on macOS):

```bash
git clone https://github.com/GrantPukka/loupe && cd loupe
make web && make build     # make build alone skips the browser UI
./loupe demo
```

`go install github.com/GrantPukka/loupe/cmd/loupe@latest` also works. The
frontend is not committed, so a binary installed that way has the CLI, the TUI,
and the HTTP API but no browser UI — `--ui` will tell you so rather than fail
oddly.

Windows: use WSL for now.

## Filtering

```
level:error                     one level
level:>=warn                    warn and above
14:00-15:00                     a time window, in your local timezone
last:15m                        relative to the newest record, not to now
                                (in --follow, relative to the wall clock)
source:nginx                    one source
-source:nginx                   everything else
status:>=500 latency_ms:>1000   any field, promoted or nested
trace_id:a91c40f2               one request across every service
                                (`loupe trace` puts it on a timeline)
pattern:9acf7d11271f            every record sharing a message template
"read timed out"                exact phrase
```

Everything above composes. `loupe sql "SELECT ..."` drops to raw DuckDB when you
need more. Full syntax: [docs/FILTER-DSL.md](docs/FILTER-DSL.md).

## Watching an incident happen

`--follow` keeps reading as records are written, through the same filter:

```bash
loupe ./logs 'level:>=error' --follow    # the terminal
loupe tui ./logs --follow                # the full-screen view
loupe serve ./logs                       # the browser — click ● live
```

New records go through the same compiled filter as everything else, so a live
view and the same query run afterwards can never disagree. In the browser, the
pattern rail and the live stream are both off until you switch them on: opening
a page should not start polling your log directory.

In follow mode `last:15m` counts back from the wall clock rather than from the
newest record, because the newest record is a moving target while records are
arriving.

## Reading from a pipe

Anything that writes logs to stdout can be read directly, and records appear as
they arrive rather than after the pipe closes:

```bash
kubectl logs -f api | loupe 'level:>=error'
docker logs -f web | loupe
journalctl -f | loupe
zcat old.log.gz | loupe          # gzip is detected from the content
loupe ./logs -                   # a pipe and a directory on one timeline
```

A bare `loupe` with something piped into it reads the pipe. A bare `-` names it
explicitly, which is what lets it compose with real paths.

Four things worth knowing:

- **One pipe is one format.** A directory gives every file its own parser. A
  pipe is a single source, so it gets a single detected one — right for
  `kubectl logs api`, which is one service writing one format, and wrong for
  concatenating a whole directory into it. On the six-format demo data:

  ```bash
  cat ./logs/*.log | loupe     # 138,949 of 212,878 records unparsed
  loupe ./logs                 # 1,499 unparsed — a parser per file
  ```

  No line is dropped either way, and an unparsed one stays searchable as text.
  But its fields are never extracted, so a field filter quietly answers from
  the one format that did parse: `status:>=500` finds **7,267** records through
  that pipe and **14,480** through the directory, and nothing on screen says
  half the answer is missing. Point loupe at the directory, and pipe only what
  genuinely arrives on a pipe.

- **A stream is never cached.** The same bytes will not be there to re-read, so
  every run pays full price. The status line says so.
- **The filter is resolved against the records that have arrived.** A field that
  only appears later is an error rather than an empty result, and the message
  says why.
- **`patterns`, `histogram`, `sql`, `sources`, `tui`, and `serve` read the pipe
  to the end first.** None of them can say anything true about records that have
  not arrived. They print a line saying they are waiting for the pipe to close.

## Following one request

```bash
loupe trace a91c40f2 ./logs
```

```
  00:16:00.000          auth-svc       info  token validated
▸ 00:16:00.632  +632ms  checkout-api   info  request completed
  00:16:00.700   +68ms  payment-worker error PaymentGatewayException: read timed out after 3000ms …

Span 700ms, of which 632ms waiting before checkout-api.
access, postgresql, syslog never record trace_id, so this trace cannot say
whether the request reached them.
```

The wait between hops is usually the finding — five lines that all look fine
and one long pause between two of them — so it is measured, and the longest one
is marked.

The correlation field is detected (`trace_id`, `traceId`, `request_id`,
`req_id`, `x-request-id`, `correlation_id`) by which one covers the most
records, and the choice is printed. `--field` names it yourself.

**Two kinds of silence, kept apart.** A service that records correlation ids
and has none for this trace probably did not handle the request. A service that
records none at all — Nginx combined has nowhere to put one — may have handled
it and simply cannot say. Reported as one category, you would conclude a request
skipped services it went straight through, so they are reported separately.

Records with no timestamp still belong to the trace: they are listed last, in
ingest order, and counted. A worker that died mid-write is exactly the record
you need.

`--handoff` exports the timeline and its disclosures as a pasteable extract, and
in the browser any trace value in a record's detail has a **→ trace** button.

## Grouping messages into patterns

Thirty-four thousand lines are usually a dozen shapes with the values filled in
differently. `loupe patterns` collapses them and counts each shape:

```bash
loupe patterns ./logs                      # every template, most frequent first
loupe patterns ./logs 'level:>=error'      # templates within a filter
loupe patterns ./logs --new-since 15m      # only shapes with nothing older
loupe patterns ./logs --all                # the whole long tail
```

```
002cf356a676  62,549  request completed
21ff829cbed3  14,237  POST /api/orders/<num>
9acf7d11271f   4,397  PaymentGatewayException: read timed out after <num>ms
```

Only value-shaped tokens are masked — numbers, uuids, addresses, quoted
strings, path segments, timestamps, hex ids. A bare word is never touched, so
`/api/cart` and `/api/checkout` stay separate templates: which endpoint is
failing is usually the whole finding.

The id on the left is a filter term. Paste it back to see the records behind a
template, short-hash style like git:

```bash
loupe ./logs 'pattern:9acf7d11271f'
loupe ./logs 'pattern:9acf7d'
```

An id that is not in the data is an error with the nearest matches, never an
empty table. In the browser, press `p` for the same list as a clickable rail.

## Timezones

Logs are in UTC. You are not. loupe shows times in your timezone and prints the
conversion, so nobody does offset arithmetic at four in the morning:

```
Times shown in Europe/London (BST, UTC+01:00)
Window: 02:00–07:30 BST  =  01:00–06:30 UTC  ·  Wed 2026-08-13
1,204 records · 18 excluded (no timestamp)
```

Sources whose format carries no timezone are flagged as assumed rather than known,
and clock changes inside a window are reported rather than silently mishandled.

## Handing findings to someone else

```bash
loupe ./logs '02:00-07:30 level:>=error' --handoff incident.md
```

Produces a pasteable extract with the window in both timezones, the query, the
source files and their timezone assumptions, the matched records, and the raw
lines. See [docs/HANDOFF.md](docs/HANDOFF.md).

A handoff describes a finished read, so it is refused on a live pipe: redirect
the stream to a file first, then hand off from that.

## Formats

Full fidelity — every field promoted and filterable:

- JSON lines · logfmt

Known structure — fixed fields extracted:

- Nginx / Apache combined and common · syslog RFC5424 · Postgres · Log4j

Best effort — timestamp, level where present, message as text:

- Anything else. The line is kept whole and stays searchable, and any timestamp
  in it still lands on the timeline.

**Your format missing?** It is about a hundred lines and a fixture to add one —
see [CONTRIBUTING.md](CONTRIBUTING.md). This is the most useful contribution to
the project.

## Try it without any logs

```bash
loupe demo
```

Generates a realistic incident across six sources in six formats — a Postgres pool
exhaustion cascading into app errors and 502s — then opens the UI on it. The same
trace id runs through all six, which is the part worth looking at.

It writes into the cache directory, never the directory you are standing in. Add
`--print` to stay in the terminal, `--regenerate` for fresh data.

## What this is not

Not a Datadog or Splunk replacement. No alerting, no dashboards, no accounts, no
agent, no server. It reads files on your machine and shows them to you.

## License

MIT
