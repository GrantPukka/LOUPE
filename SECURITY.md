# Security

## The threat model

People point loupe at production logs during incidents. Those logs contain
tokens, session identifiers, internal hostnames, customer records, and worse.
The tool's security posture is mostly a set of things it refuses to do:

- **No network.** loupe makes no outbound connections — no telemetry, no update
  check, no crash reporting, no share links. It does not phone home, and there is
  no configuration option to make it.
- **Read-only.** It never modifies, deletes, or writes to a source log file. The
  only things it writes are the cache directory, the subscriptions file, and an
  explicit `--handoff` output.
- **Local only.** `loupe serve` and `--ui` bind loopback and refuse to bind
  anything else. There is no authentication because there is nothing remote to
  authenticate.
- **No accounts, no sync, no server.** Nothing leaves the machine unless you
  export it yourself.

A bug that breaks any of those is a security bug, not a feature request. The
clearest example: **if you find a way to make loupe open a network connection,
report it.**

## Reporting a vulnerability

Use GitHub's private reporting — the **Security** tab, then **Report a
vulnerability**. That opens a private advisory visible only to maintainers.

Please do not open a public issue for anything that would let someone read data
they should not.

Include what you would put in a bug report: version, platform, and the smallest
input that reproduces it. **Scrub any real log data** — a synthetic sample that
triggers the same path is better than a real one.

Expect an acknowledgement within a week.

## Scope

In scope:

- Any outbound network connection
- Writing to, modifying, or deleting a source log file
- `serve` binding to a non-loopback interface, or accepting cross-origin
  requests that let another page in the browser read your logs
- Path traversal out of the directories you named, including through the folder
  browser and subscriptions
- SQL injection through the filter DSL. Every term compiles to parameterised
  SQL; a way to reach string concatenation is a real finding
- Secrets surviving `--redact` in a handoff
- Reading a crafted log file causing code execution

Out of scope:

- Denial of service by feeding it an enormous or malformed file. It should fail
  cleanly, and a crash is worth an ordinary bug report, but it is not a
  vulnerability — you already had the file
- Anything requiring an attacker who can already run code as your user
- The `loupe sql` command executing arbitrary SQL. That is what it is for, and
  the database is a local read-only copy

## Supported versions

Pre-1.0. Fixes land on `main` and in the next release; there are no backports.
