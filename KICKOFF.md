# Kickoff plan

How to drive this project with Claude Code without ending up with 8,000 lines you
don't understand. Delete this file before making the repo public.

**The rule that matters:** one session, one milestone, tests green, you read the
diff, you commit. Do not let a session sprawl across three layers — that is how
you end up unable to review your own project, which is fatal for a repo that needs
you to answer issues about it later.

---

## Session 0 — de-risk the stack (30 minutes, do this first)

Before writing anything real, prove the one assumption that could invalidate the
whole plan:

```
Create a minimal Go program that imports github.com/marcboeker/go-duckdb, opens an
in-memory database, creates a table, inserts 100k rows via the Appender API, and
queries them. Report the build time, the binary size, and whether it builds
cleanly on this machine.
```

If CGO fights you here, you want to know now, not in week three. Binary should land
around 40–60MB. If it doesn't build, stop and reconsider before going further.

Then set up the repo:

```
Initialise a Go module github.com/GrantPukka/loupe with the package layout described
in ARCHITECTURE.md section 4. Create empty packages with doc comments only, a
.gitignore for Go, and a Makefile with build, test, lint, and demo targets. No
implementation yet.
```

---

## Session 1 — parse

```
Read CLAUDE.md and ARCHITECTURE.md sections 3.1 and 3.2.

Implement internal/source (Source interface, local file, directory walk, stdin,
transparent gzip) and internal/parse (Parser interface, registry, detection) with
two parsers: jsonl and logfmt. Golden-file tests for both, using fixtures I will
generate with cmd/blaster.

Do not touch internal/store or the query layer yet.
```

Generate fixtures first: `go run ./cmd/blaster -out ./testdata/mixed -seed 7`.

**Checkpoint:** the parsers correctly reject the malformed lines the blaster
injected, and the counts match `testdata/mixed/manifest.json`.

---

## Session 2 — store

```
Read ARCHITECTURE.md section 3.4. Implement internal/store: DuckDB lifecycle, the
logs table schema, batched ingest via the Appender API, and a Query method taking
parameterised SQL. Then wire cmd/loupe so `loupe sql "SELECT ..."` works end to end
against a directory. Table and JSON renderers in internal/render.

No cache layer yet, no filter DSL yet.
```

**Checkpoint:** `loupe sql "SELECT level, count(*) FROM logs GROUP BY 1"` returns
correct counts over the demo directory. This is the first moment the project is
real — stop and play with it.

---

## Session 3 — the filter DSL (the big one, give it a whole session)

```
Read docs/FILTER-DSL.md in full. Implement internal/query: lexer, parser, AST, and
SQL compiler for every term type in sections 3 through 6. Leave time terms
(section 2) unimplemented for now — stub them and return a clear error.

Parameterised SQL only. Round-trip tests for every term type. Unknown field names
produce an error with a spelling suggestion, never an empty result.
```

This is the highest-risk session for over-engineering. Read this diff carefully.

---

## Session 4 — time and timezones

```
Read docs/FILTER-DSL.md section 2 in full, including the DST and assumed-timezone
subsections. Implement the time terms, the display timezone layer, the conversion
banner, and --source-tz.

Tests must cover: DST spring-forward gap, autumn-back ambiguity, a source with no
offset, a query spanning midnight, and last: resolving against the newest record
rather than wall clock.
```

Separate from session 3 on purpose. Timezone bugs are subtle and you want this diff
by itself.

---

## Session 5 — the rest of the formats

```
Add parsers for nginx-combined, syslog RFC5424, postgres, and log4j, including
log4j multi-line stack trace continuation. Golden fixtures for each from
cmd/blaster. Then implement format auto-detection across a mixed directory and
verify a single loupe invocation over ./demo produces one correctly ordered
timeline from all six sources.
```

**Checkpoint:** the cross-source correlation demo works. `loupe ./demo
'trace_id:...'` shows one request across four formats. This is the project's whole
pitch — if it feels good here, you're on track.

---

## Session 6 — cache and schema inference

```
Implement the fingerprint cache described in ARCHITECTURE.md 3.4, and dynamic
field promotion from internal/schema. Benchmark cold and warm open on the demo
directory and report both.
```

---

## Session 7 — HTTP API

```
Read ARCHITECTURE.md section 5. Implement internal/server with the four endpoints,
calling the same query path as the CLI. No frontend yet — verify with curl.
```

---

## Session 8 — the UI

Give it the mockup file. That is the spec.

```
Read ARCHITECTURE.md section 6 and the attached mockup HTML, which is the design
reference — match its layout, colour semantics, density, and interactions.

Build it as Preact + Vite in web/, built to web/dist and embedded via go:embed.
Virtualised row list. One screen. The timeline drag must write a real DSL string
into the filter box.

Do not add features that are not in the mockup.
```

That last line is load-bearing. This is where scope creep lives.

---

## Session 9 — handoff

```
Read docs/HANDOFF.md. Implement --handoff for markdown and json, generated from
the same query AST as the display path.
```

---

## Session 10 — release

```
Set up GitHub Actions: test and lint on push; release workflow building
darwin/linux on amd64 and arm64 via a native runner matrix, attaching binaries to
tagged releases. Add a Homebrew tap formula. Note the CGO cross-compilation
constraint in a comment.
```

---

## Session 11 — launch prep (do not skip)

- Record the GIF. Ten seconds: wide view → red cluster → drag → click row → click
  a field → filtered. This is the single highest-leverage hour of the project.
- Write the 15 `good first issue` tickets from CONTRIBUTING.md, each with a sample
  of the target format and a link to `logfmt.go` as the template.
- `loupe demo` working end to end from a clean install.
- Delete this file.
- Then post: r/golang, r/devops, Lobsters, Hacker News "Show HN".

---

## Ongoing habits

- **Start each session by naming the docs to read.** Context resets; the docs are
  how the invariants survive.
- **Read every diff.** If you don't understand a file, you cannot review a PR that
  touches it in six months.
- **When it proposes an abstraction, push back once.** The default answer is a
  switch statement.
- **Run `loupe` on your own real logs weekly.** Being your own user is the only
  reliable source of good decisions here.
