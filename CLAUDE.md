# CLAUDE.md

Instructions for Claude Code working in this repository.

**Read before structural work:** `ARCHITECTURE.md`. Before touching
`internal/query/` or anything time-related: `docs/FILTER-DSL.md`. Before touching
export: `docs/HANDOFF.md`.

---

## The project

`loupe` is a single-binary log explorer. Point it at a directory of log files in
mixed formats, get fast SQL, a filter language, and a local web UI, with no
external services. Go + DuckDB. Read-only, local-first.

The user is a developer or an on-call engineer with messy logs on disk who does
not want to stand up Elasticsearch. Often they are under time pressure. Every
decision serves that.

---

## Hard invariants

Do not violate these without asking. If a task seems to require it, stop and say so.

1. **Single static binary.** No runtime dependencies, no sidecar processes, no
   Docker requirement, no `npm install` at runtime. Frontend assets are built at
   release time and embedded via `//go:embed`.
2. **CLI before UI.** Any capability must exist and be tested in the CLI before it
   appears in the web UI. The UI calls the same code paths, never its own.
3. **Read-only.** Never modify, delete, or write to source log files. The only
   writes are the cache directory and explicit `--handoff` output.
4. **No network.** The tool makes no outbound connections. No telemetry, no update
   check, no share links. This is what makes it trustworthy with production logs.
5. **Zero-config happy path.** `loupe ./logs` must work on a messy directory with
   no flags. New flags need justification; make the default smarter instead.
6. **Dependency direction is one-way.** `parse` must not import `store`, `query`,
   or `server`. `store` must not import `server`.
7. **The `Parser` interface stays tiny.** It is the contribution surface for
   outside contributors. Do not add methods to it to solve a problem local to one
   parser.

---

## Never lose data silently

This is the project's core correctness principle and the source of most of its
specific rules. A log tool that quietly drops records is worse than no log tool,
because it produces confident wrong conclusions during incidents.

- A malformed line never aborts a file. Keep the raw text, mark it unparsed.
- Records with no parseable timestamp are still ingested and still queryable
  (`ts:none`). Any time filter reports how many it excluded for that reason.
- Unrecognised fields go into the `fields` bag. Never drop a key.
- Truncated output always says it was truncated, and states the full count.
- Assumed timezones are surfaced in the status line and in every handoff.

---

## Time and timezones

Full rules in `docs/FILTER-DSL.md` §2. The ones that are easy to get wrong:

- **One display timezone per session**, defaulting to the system zone, always
  visible on screen. Bare times in queries are interpreted in it.
- **Always print the conversion** — local window and UTC window, before results.
  The user should never do offset arithmetic.
- **`last:15m` is relative to the newest record in the loaded data**, not wall
  clock, except in `--follow` mode.
- **Bare times resolve against the data's date range**, and the resolved date is
  printed. Never resolve silently.
- **Sources with no timezone in their format default to assumed UTC**, overridable
  with `--source-tz`, and the assumption is always disclosed.
- **Detect DST transitions inside a window** and report them. Never silently pick
  one of two ambiguous local times.
- Use `time.LoadLocation` and the OS tzdata. Never store or compute fixed offsets.

---

## Filter DSL

- Lexer → parser → AST → **parameterised SQL**. Never build SQL by string
  concatenation, however small the change looks.
- Resolve all time terms to a single interval at AST level before compiling.
- An unknown field name is an error with a spelling suggestion and a list of
  available fields — never an empty result set.
- The UI's timeline drag writes a real DSL string into the filter box, so the
  interaction teaches the syntax and stays shareable.
- Every term type needs a round-trip test: `parse(render(ast)) == ast`.
- A `stats` clause compiles through the same AST to parameterised SQL. Time bins
  are anchored to local midnight in the display timezone, never to the epoch, and
  an aggregation states every record it could not place.

---

## Adding dependencies

Ask first. The allowed set is `cobra`, `go-duckdb`, a colour library, and the
standard library. Prefer stdlib. Do not add a logging framework, a DI container, a
config library, or an ORM.

---

## Code style

- `gofmt`, `go vet`, `golangci-lint run` clean.
- **Return errors, wrap with context, never swallow.**
  `fmt.Errorf("parse %s: %w", name, err)`.
- Accept interfaces, return structs.
- **No premature abstraction.** Two implementations is not a pattern. Write the
  concrete thing; abstract on the third occurrence. This is the most common way
  you make this codebase worse — you tend to produce a factory, a registry, and a
  strategy interface for something that needed a switch statement.
- **No unnecessary configuration knobs.** Try to make the default correct first.
- Comments explain *why*. Do not narrate the code.
- Keep functions under ~50 lines.

---

## Testing

- Table-driven, standard library `testing`, no assertion framework.
- **Every parser needs a golden-file test.** Fixtures in `testdata/<format>/`,
  regenerate with `go test ./... -update`.
- **Fixtures must be messy**: blank lines, truncated final lines, mixed timestamp
  formats, at least one malformed record. `cmd/blaster` generates these — use
  `-seed` for deterministic output.
- **Do not mock DuckDB.** Use a real in-memory instance; it is fast enough.
- **No network access in tests, ever.**
- Timezone tests must cover: DST spring-forward gap, autumn-back ambiguity, a
  source with no offset, and a query spanning midnight.
- Run `go test ./...` before claiming a task is done. If tests fail, fix the code
   — do not adjust assertions to match broken output.

---

## Git and commits

- **Do not add any AI attribution to commits or pull requests.** No
  `Co-Authored-By` trailer, no "Generated with Claude Code" footer, no emoji,
  nothing in the PR body. Commit messages contain only the change description.
  This is also set in `.claude/settings.json`; honour it regardless of settings
  state.
- Conventional commits: `feat:`, `fix:`, `perf:`, `refactor:`, `docs:`, `test:`,
  `chore:`. Scope where useful: `feat(parse): add syslog RFC5424 parser`.
- Subject under 72 characters, imperative mood, no trailing period.
- **One logical change per commit.** Do not bundle a refactor with a feature.
- **Do not commit unless asked.** Do not push, tag, or open a PR unless asked.
- Never commit `web/dist/` by hand. Never commit fixtures over 1MB.

---

## Working style

- **Plan before large changes.** Anything touching more than three files or adding
  a package: outline the approach and wait for confirmation.
- **Smallest change that works.** Do not refactor adjacent code you were not asked
  to touch.
- **When a spec is ambiguous, ask.** Do not build 400 lines on a guess.
- **Say when something is a bad idea.** If a request conflicts with an invariant
  or causes a problem later, say so before implementing.
- Do not add unrequested features.

---

## Performance expectations

State these when relevant to a change; do not optimise speculatively.

- 1GB of JSON lines ingests in under 20 seconds on a modern laptop.
- A cached re-open of the same directory is under 200ms.
- A filter over 10M rows returns the first page in under 500ms.
- Memory stays bounded regardless of input size — stream, never load a whole file.

If a change plausibly affects ingest throughput, benchmark before and after and
report both numbers.

---

## Common mistakes in this codebase

- Building the filter DSL with string concatenation instead of a real parser.
- Loading an entire log file into memory to "make parsing simpler."
- Adding a method to `Parser` to accommodate one awkward format.
- Tests against a mocked store that pass while the real SQL is invalid.
- A web UI endpoint with no CLI equivalent.
- Treating a missing timestamp as a fatal error rather than a zero value.
- Computing timezone offsets arithmetically instead of via the tz database.
