<!-- Thanks for this. Nothing below is bureaucracy — each line is something
     that has caused a real problem in this repo before. -->

## What this changes

<!-- One or two sentences. If it is a new parser, say which tool and version
     produces the format, and where the sample lines came from. -->

## Why

<!-- What was wrong or missing. For a bug fix, the failing case is enough. -->

---

- [ ] `make check` passes (gofmt, vet, golangci-lint, tests)
- [ ] New behaviour has a test
- [ ] If the UI changed: `make e2e` passes
- [ ] If anything about ingest changed — schema, parser output, level
      normalisation, timestamp handling, promotion rules — `IngestVersion` in
      `internal/store/cache.go` is bumped

<!-- That last one is the easiest way to introduce a silent bug here. Nothing
     breaks on your machine, because you re-ingested while developing. Everyone
     with a warm cache keeps reading records produced by the code you just
     changed, with no warning. -->

- [ ] No AI attribution in commits (no `Co-Authored-By`, no generated-with
      footer)

<!-- If this adds a dependency, please open an issue first. The allowed set is
     cobra, go-duckdb, a colour library, and the standard library. -->
