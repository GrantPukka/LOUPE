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
"read timed out"                exact phrase
```

Everything above composes. `loupe sql "SELECT ..."` drops to raw DuckDB when you
need more. Full syntax: [docs/FILTER-DSL.md](docs/FILTER-DSL.md).

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
