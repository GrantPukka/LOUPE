# Filter DSL specification

Supersedes §3.5 of `ARCHITECTURE.md`. Save as `docs/FILTER-DSL.md`.

This is the primary interface to the tool. Getting it right matters more than
anything in the UI, because it is what people type, save, and paste to each other.

---

## 1. Grammar

```
query   := term*
term    := ['-'] ( time | field | free )
time    := ('after'|'before'|'between'|'last'|'on') ':' timeexpr
field   := key ':' [op] value
key     := bare | '"' quoted '"'
op      := '>=' | '<=' | '>' | '<' | '~'
value   := bare | '"' quoted '"' | value ',' value
free    := bare | '"' quoted '"'
```

**Terms are joined by AND.** Commas within a value mean OR. There are no
parentheses and no `OR` keyword in v1 — this is deliberate. Every log tool that
adds boolean grouping ends up with a query language nobody can remember, and the
escape hatch already exists: drop to `loupe sql` for anything this can't express.
Revisit only if issues actually ask for it.

Whitespace separates terms. Order is irrelevant. Terms of the same kind intersect
rather than override, so `after:14:00 before:15:00` and `between:14:00-15:00` are
equivalent, and `after:14:00 after:14:30` means 14:30 onward.

---

## 2. Time

The four requested filters, plus the forms people will reach for by reflex.

| Form | Meaning |
|---|---|
| `between:14:00-15:00` | Inclusive of the start, exclusive of the end |
| `14:00-15:00` | Bare range, no keyword — same as above |
| `after:14:00` / `since:14:00` | `since` is an alias; both accepted |
| `before:15:00` / `until:15:00` | |
| `last:15m` `last:2h` `last:3d` | Relative window — see the trap below |
| `on:2026-08-13` | That whole calendar day |
| `after:2026-08-13T14:00:00Z` | Full RFC3339 for scripts and permalinks |
| `between:14:00-14:05:30` | Seconds allowed; precision is optional throughout |

Accepted bare-time shapes: `14:00`, `1400`, `14:00:00`, `2:00pm`. Accept all four;
they cost twenty lines of parsing and remove a whole class of "why didn't this
work" issues.

### 2.1 Which day does a bare time mean?

A bare `14:00` resolves against **the date range of the loaded data**, not today.
If the data spans one calendar day, use that day. If it spans several, use the
most recent day containing that time, and **print which date was chosen** in the
status line. Never guess silently.

If the resolved range matches zero records but the data does contain that time on
a different day, say so explicitly rather than returning an empty table:

```
No records in 14:00–15:00 on 2026-08-13.
Data covers 2026-08-11 to 2026-08-13 — try on:2026-08-11 14:00-15:00
```

### 2.2 The `last:` trap

`last:15m` must be relative to **the newest record in the loaded data**, not to
wall-clock now. Otherwise `last:15m` on a log file from yesterday returns nothing,
which is the single most confusing possible result and will generate issues within
a week of launch.

Provide `--relative-to=now` for the follow/live case, and make the status line
state which anchor was used. In `--follow` mode, default to wall clock.

### 2.3 Timezones

Logs are usually UTC. People think in local time. This mismatch is where log tools
lose users' trust, so the rule must be explicit and visible, never inferred.

- **One display timezone for the whole session**, defaulting to the local zone.
- **Bare times in queries are interpreted in the display timezone.**
- **Explicit offsets in queries win** (`after:2026-08-13T14:00:00Z`).
- `--utc` switches display and interpretation to UTC. `--tz=Europe/London` sets it
  explicitly.
- **The active timezone is always visible** — in the CLI status line, and in the
  UI next to the timeline. A user must never have to guess whether the times on
  screen are theirs or the server's.
Use the OS timezone database via `time.LoadLocation`, never a stored fixed offset.
That is what makes the DST handling below correct for free.

**Always show the conversion.** Before any results, print the window in both zones:

```
Times shown in Europe/London (BST, UTC+01:00) — from system clock
Window: 02:00–07:30 BST  =  01:00–06:30 UTC  ·  Wed 2026-08-13
1,204 records · 18 excluded (no timestamp)
```

This is the feature, not a nicety. Someone working an incident at 04:00 should
never have to do offset arithmetic, and the UTC line is what they paste into the
ticket.

### 2.4 DST boundaries inside a window

A window that crosses a clock change is not the duration it appears to be, and a
local time inside the changeover either does not exist or occurs twice. In the UK
the change lands at 01:00 UTC, which is squarely inside the kind of overnight
window this tool gets used for.

Detect a transition inside the resolved window and say so:

```
Note: clocks changed at 02:00 BST → 01:00 GMT on 2026-10-26.
Window 02:00–07:30 local spans 6h30m of real time, not 5h30m.
01:00–06:30 UTC.
```

For a local time that does not exist (spring forward), resolve to the instant the
clock jumped to and say so. For one that occurs twice (autumn back), include both
occurrences and say so. Never pick one silently.

### 2.5 Sources that record no timezone

This is the trap that silently corrupts an investigation. Log4j, Postgres, and
most application logs write bare local time with no offset. If the server runs UTC
and the operator's laptop is on BST, every one of those records is displayed an
hour out and nothing warns anybody.

- `--source-tz=UTC` sets the assumed zone for all timezone-less sources.
- `--source-tz=payment-worker:UTC,postgres:Europe/London` sets it per source.
- Default assumption is UTC, not local — servers overwhelmingly run UTC, and the
  wrong default here is worse than a slightly surprising one.
- `loupe sources` lists every file with its format and whether its timezone is
  **known** (carried in the format) or **assumed** (defaulted), so the assumption
  is auditable in ten seconds.
- When a query returns records from a source with an assumed timezone, the status
  line says so once.

### 2.6 Clock skew between hosts

Records from different sources sharing a `trace_id` should be within a second or
so of each other. When they are not, a host has drifted, and during an incident
that misdirection can waste hours.

`loupe skew` compares timestamps across sources for shared trace ids and reports
apparent offsets:

```
nginx            reference
checkout-api     +0.04s   (n=1,204)
payment-worker  +41.20s   (n=118)   ← likely NTP drift
```

This is a v2 feature. Note it in the roadmap, do not build it in M1, but keep
`trace_id` correlation in the data model from the start so it stays possible.

### 2.4 Records with no parseable timestamp

These exist — malformed lines, formats without timestamps, continuation lines. A
time filter necessarily excludes them, and **silently dropping them is a bug**.

- Any time filter reports the count it excluded for this reason:
  `1,204 records · 18 excluded (no timestamp)`.
- `ts:none` selects exactly those records so they can be inspected.
- Without a time filter, they are always shown, sorted to the position of the
  surrounding lines in their source file.

---

## 3. Severity

`level` is ordinal, so it supports comparison as well as equality:

```
level:info                 only info
level:error,fatal          either
level:>=warn               warn, error, and fatal
-level:debug               everything except debug
```

Ordering: `trace < debug < info < warn < error < fatal`. Normalise on ingest, so
`WARNING`, `warn`, and `W` all compare equal. If a source uses a level outside
this set, keep the original string, sort it above `trace`, and match it only on
exact equality.

---

## 4. Source

```
source:nginx               logical source name
source:nginx,postgres      either
-source:nginx              exclude
file:access.log            the specific file
file:access.log*           glob, catches rotated files
format:jsonl               everything parsed by a given parser
```

`source:` should match on a prefix when unambiguous, so `source:check` finds
`checkout-api`. Ambiguous prefixes error and list the candidates.

---

## 4.1 Message templates

```
pattern:72537a34170e       every record sharing a message template
pattern:72537a            a unique prefix of the id, like a git short hash
-pattern:72537a34170e      exclude that template
pattern:none               records with no template at all
```

The id is what `loupe patterns` prints beside each template. It names the
template's text, so it is stable across runs and across machines without
anything being stored between them — the same shape always gets the same id.
`pattern:` takes the id, not the template's text; the text can contain spaces
and masks and would be miserable to type.

**An id that is not in the loaded data is an error, not an empty result.** This
is the opposite of how `source:` behaves, deliberately. A source name is
something the user knows from outside the data, so "is nginx in here?" is a real
question with "no" as a real answer. A template id only ever comes from a
listing of this same data, so an id that is not present is a typo or a stale
paste. The error suggests the nearest ids by prefix, which is what a mistyped
id looks like.

Value-shaped tokens are masked when the template is derived: numbers, uuids,
addresses, quoted strings, path segments, timestamps, and hex ids. A bare word
is never masked, so two messages differing by a word stay two templates.

---

## 5. Message and free text

```
timeout                    bare word: message and all field values
"read timed out"           quoted phrase, exact substring
message~timeout            substring, message only
message~/^GET \/api/       regex, delimited by slashes
-message~healthz           exclude
```

Bare words search message *and* fields, because that's what people expect from a
search box. `message~` restricts to the message, for when a value elsewhere is
producing noise. Matching is case-insensitive unless the pattern contains an
uppercase character — the "smart case" behaviour from `ripgrep`, which users of
this kind of tool already have in their fingers.

---

## 6. Arbitrary fields

Any promoted column or JSON field, by name:

```
status:>=500  latency_ms:>1000  user_id:u_4471  trace_id:a91c40f2  region:eu-west-1
```

Numeric comparison when both sides parse as numbers, string comparison otherwise.
`field:*` matches records where the field exists at all; `field:none` where it
doesn't.

### 6.1 Field names that need quoting

A field name comes out of a log file, so it can contain anything — a space, a
colon, a quote, a control byte. Write such a name in double quotes:

```
"weird\"key":y            a name containing a quote
"a key with spaces":y     a name containing whitespace
"key:with:colons":y       a name containing colons
```

**A quoted key is taken literally**, which is also how you reach a field whose
name collides with the DSL's own vocabulary:

```
last:15m                  the time filter
"last":15m                a field actually named last, whose value is 15m
```

Escapes inside a quoted key are the same as in a quoted value: `\"` for a quote
and `\\` for a backslash. Rendering quotes a key whenever a bare one would parse
back as something else, so a filter built by the UI round-trips unchanged.

Without this, a record carrying such a field is ingested and displayed but
cannot be filtered on — a silent hole in the data, which is exactly what this
project's first principle forbids.

### 6.2 Values that need quoting

The same rule applies to the right of the colon. A value is quoted when a bare
one would read back as something other than itself:

```
A:"=<>"                   a value that begins with an operator character
A:"~x"                    a value that begins with a tilde
after:"14:00 x"           a time expression containing whitespace
```

Quoting is about surviving the round trip, not about being valid: the last one
renders and parses back unchanged, then fails when it is resolved, because
`14:00 x` is not a time. That is the right order — the error names the real
problem instead of the filter quietly becoming `after:14:00`.

The one worth knowing is the first. `A:=>` is an explicit equals against the
literal `>`, so it renders as `A:">"` — bare, it would read as a comparison with
no value.

A bare time range is the exception: `14:00-15:00` has no keyword to sit outside
the quotes, so it cannot be quoted at all. Anything that would need it is read
as an ordinary field term instead, or rejected with a message saying so. Write
`between:"..."` when the expression is unusual.

**The rule, in one line: rendering must produce text that parses back to the
same filter.** The UI writes rendered filters into the box, so a filter that
changed under a round trip would change under the user without saying so.

---

## 7. Errors

The most common failure is a typo'd field name silently returning zero rows.
**Do not return an empty result for an unknown key.** Error, and suggest:

```
unknown field "sevrity"
did you mean: severity, service
fields present in this data: ts, level, service, status, latency_ms, trace_id, …
```

Same for unparseable times: name the value and show two working examples. An
error message that includes a copyable correct example is worth more than any
amount of documentation.

---

## 8. Worked examples

The line from the original question, plus the cases that must be tested:

```
14:00-15:00 level:info source:nginx
    info-level nginx records between 14:00 and 15:00 in the display timezone

between:14:00-15:00 level:>=warn -source:nginx timeout
    everything at warn or above, excluding nginx, mentioning "timeout"

last:15m level:error status:>=500
    relative to the newest record loaded, not to now

on:2026-08-13 14:11-14:14 trace_id:a91c40f2
    one request's path across every source, during the incident window

ts:none
    records the parser could not extract a timestamp from
```

---

## 9. Implementation notes

- **Lexer → parser → AST → parameterised SQL.** Not string concatenation. The
  time handling above cannot be done correctly with regex substitution, and
  string-building queries is how injection bugs and unfixable precedence problems
  arrive.
- Time terms compile to `ts >= ? AND ts < ?`, which is index-friendly. Resolve all
  time terms to a single interval at AST level before compiling, so overlapping
  terms intersect rather than producing redundant predicates.
- The UI's timeline drag must produce a **real DSL string** and put it in the
  filter box — not a hidden internal range. Dragging teaches the syntax, and the
  result stays copyable and shareable. This is the single highest-value detail in
  this document.
- Round-tripping matters: `parse(render(ast)) == ast`. Test it.

---

## 10. Append to CLAUDE.md

```md
## Filter DSL

`docs/FILTER-DSL.md` is the specification. Read it before touching
`internal/query/`. Rules that are easy to get wrong:

- The DSL is lexer → parser → AST → parameterised SQL. Never build SQL by string
  concatenation, however small the change seems.
- `last:15m` is relative to the newest record in the loaded data, not wall clock,
  except in --follow mode.
- Bare times resolve against the data's date range, and the resolved date is
  printed to the user. Never resolve silently.
- One display timezone per session, always visible on screen. Bare times are
  interpreted in it. Explicit offsets in the query win.
- Time filters must report how many records they excluded for having no
  timestamp. Silently dropping those records is a bug, not an edge case.
- An unknown field name is an error with a suggestion, never an empty result set.
- Timeline drag in the UI writes a real DSL string into the filter box.
- Every term type needs a round-trip test: parse(render(ast)) == ast.
```
