package parse

import (
	"errors"
	"sync"
	"sync/atomic"
)

func init() { Register(&mixedParser{}) }

// MixedName is the format identifier for per-line detection.
const MixedName = "mixed"

// mixedParser detects the format of every line rather than of every file.
//
// loupe's headline promise is that a pile of unlike logs becomes one timeline,
// and the design that delivered it — sample the head of a file, pick a parser,
// use it for the whole file — quietly assumed that a file is one format. A
// merged stream is not. `journalctl`, a Kubernetes log collector, and
// `cat *.log > combined.log` all produce a single file carrying many formats,
// and on one of those the file-level choice left 84.5% of records unparsed and
// therefore off the timeline entirely: a text search wearing a log explorer's
// clothes. The documented remedy, "point loupe at the directory", does not
// exist when the directory is one file.
//
// The one piece of state it holds is a cache of which parsers to try for a
// given line shape, which memoises a pure function: it changes how long Parse
// takes and never what Parse returns, so the registry's single instance is
// still safe to share and two lines the same distance into a file still parse
// the same way.
type mixedParser struct {
	// order maps a shapeKey to the parser indices to try, best first. Written
	// once per distinct shape and read on every line after that, which is what
	// sync.Map is for.
	order sync.Map // string -> []int
	// shapes counts the distinct shapes cached, so a file of pathologically
	// varied lines cannot grow the map without bound. Past the cap, detection
	// simply runs as it did before.
	shapes atomic.Int64
}

// maxShapes bounds the order cache. A merged platform log has on the order of
// twenty distinct line shapes; a thousand is far past anything a real format
// mix produces and still trivial to hold.
const maxShapes = 1024

func (p *mixedParser) Name() string { return MixedName }

// Detect never wins on its own.
//
// Choosing per-line detection is a judgement about how well the winner covers
// the file, which a confidence score cannot express: a parser can be certain
// about the 15% of lines it recognises and know nothing about the rest. That
// judgement is made in the store, which can measure coverage over the sample.
// See store.parserFor.
func (p *mixedParser) Detect([][]byte) float64 { return 0 }

// Parse offers the line to each parser in descending order of how confident it
// is about that line, and takes the first that claims it.
//
// Confidence comes from the parsers' own Detect, so a format's authors keep
// deciding what their format looks like and no priority list here has to be
// maintained alongside them.
//
// The fallback is deliberately not consulted. It claims every line and returns
// ErrNoMatch for none, so including it would report every damaged line as
// parsed and drive the unparsed count — the number that makes this tool's
// caveats visible at a glance — permanently to zero. A line no real format
// claims is unparsed here exactly as it is under any other parser: kept, marked,
// and findable. Someone who wants the fallback's best-effort guess at a
// timestamp can still ask for it with --parser text.
func (p *mixedParser) Parse(line []byte) (Record, error) {
	candidates := realParsers()
	order := p.orderFor(line, candidates)

	// A partial parse is held back rather than returned straight away. A
	// truncated JSON line is still JSON, but if some other format reads the
	// whole line cleanly then that format is the better answer, and confidence
	// order alone cannot tell the two apart.
	var (
		partial     Record
		havePartial bool
	)

	// Every parser is offered the line, in confidence order. The order is a
	// hint about which to try first; Parse is the authority on who claims it,
	// so a cached order shared by two formats costs a failed match and nothing
	// else.
	for _, i := range order {
		rec, err := candidates[i].Parse(line)
		switch {
		case err == nil:
			rec.Format = candidates[i].Name()
			return rec, nil

		case errors.Is(err, ErrPartial) && !havePartial:
			rec.Format = candidates[i].Name()
			partial, havePartial = rec, true

		default:
			// ErrNoMatch is the ordinary answer. A parser that fails some other
			// way has an opinion worth respecting, but not one worth losing the
			// line over, so both simply move on.
		}
	}

	if havePartial {
		return partial, ErrPartial
	}
	return Record{}, ErrNoMatch
}

// orderFor returns the parser indices to try for this line, best first.
//
// Scoring is the expensive half of per-line detection — one regex scan per
// registered format, on every line of the file — and it answers the same way
// for every line of the same shape. The answer is cached under shapeKey and
// the scan is skipped from the second line of a given shape onwards.
func (p *mixedParser) orderFor(line []byte, candidates []Parser) []int {
	key := shapeKey(line)
	if cached, ok := p.order.Load(key); ok {
		return cached.([]int)
	}

	// Scored into a stack array rather than a fresh slice: this runs once per
	// line shape, and the array keeps the miss path allocation-free apart from
	// the order it is about to store.
	var (
		scores [maxParsers]float64
		sample = [1][]byte{line}
	)
	for i, candidate := range candidates {
		scores[i] = candidate.Detect(sample[:])
	}

	// Highest score first, ties by registry order, which realParsers holds
	// sorted by name — so identical input always resolves to the same parser.
	// A selection sort over a dozen items beats sort.Slice's closure and
	// interface overhead.
	order := make([]int, 0, len(candidates))
	tried := [maxParsers]bool{}
	for range candidates {
		best := -1
		for i := range candidates {
			if !tried[i] && (best < 0 || scores[i] > scores[best]) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		tried[best] = true
		order = append(order, best)
	}

	if p.shapes.Load() < maxShapes {
		if _, loaded := p.order.LoadOrStore(key, order); !loaded {
			p.shapes.Add(1)
		}
	}
	return order
}

// IsContinuation asks every format that has a continuation concept.
//
// A Java stack trace is one record, and losing that in mixed mode would trade
// one kind of fragmentation for another. Asking all of them is safe because the
// shapes are distinctive — an indented `at com.example…` frame is not something
// another format's continuation rule claims.
func (p *mixedParser) IsContinuation(line []byte) bool {
	for _, c := range continuers() {
		if c.IsContinuation(line) {
			return true
		}
	}
	return false
}

// maxParsers bounds the scratch arrays Parse uses. It is a ceiling, not a
// count; registering more than this many formats is a build-time mistake that
// the panic below names immediately.
const maxParsers = 32

var (
	realOnce       sync.Once
	realCache      []Parser
	continuerCache []Continuer
)

// realParsers is every registered format except the fallback and this parser
// itself, resolved once.
//
// Once, because All takes a lock, walks the registry map and sorts the names on
// every call, and this is per line: on a 260,000-line file that dominated the
// ingest. Parsers are registered from package init and the registry is not
// meant to change afterwards, so a snapshot is the honest shape of the fact.
func realParsers() []Parser {
	realOnce.Do(func() {
		for _, candidate := range All() {
			if candidate.Name() == MixedName || candidate.Name() == FallbackName {
				continue
			}
			realCache = append(realCache, candidate)
			if c, ok := candidate.(Continuer); ok {
				continuerCache = append(continuerCache, c)
			}
		}
		if len(realCache) > maxParsers {
			panic("parse: more registered formats than maxParsers allows")
		}
	})
	return realCache
}

// continuers is the subset of realParsers with a continuation concept.
func continuers() []Continuer {
	realParsers()
	return continuerCache
}

// Coverage is the share of sample lines a parser claims outright.
//
// It is what decides whether a file needs per-line detection: a confidence
// score says how sure a parser is about the lines it recognises, which is a
// different question from how much of the file it recognises at all.
//
// The fallback is excluded from meaning anything here — it claims every line
// and parses none of them structurally — so callers must not ask about it.
func Coverage(p Parser, sample [][]byte) float64 {
	continuer, _ := p.(Continuer)

	matched, counted := 0, 0
	for _, line := range sample {
		// A continuation line is not a line this parser failed to read. It is
		// part of the record above it, and asking it to parse alone is asking
		// the wrong question — one that scores every multi-line format, Log4j
		// with its stack traces above all, as barely reading its own files.
		if continuer != nil && continuer.IsContinuation(line) {
			continue
		}
		counted++
		if _, err := p.Parse(line); err == nil {
			matched++
		}
	}

	if counted == 0 {
		return 1
	}
	return float64(matched) / float64(counted)
}

// Formats counts how many distinct formats appear in a sample, per line.
//
// Lines no format claims are counted under UnclaimedFormat, so the totals add
// up to the sample size and a caller can say what share of a file nothing
// understood. `loupe sources` reports this when a file turns out to be
// heterogeneous, so that a number like "84.5% unparsed" arrives with its
// explanation attached rather than leaving the inference to the reader.
func Formats(sample [][]byte) map[string]int {
	mixed := &mixedParser{}
	out := map[string]int{}

	for _, line := range sample {
		rec, err := mixed.Parse(line)
		// A partial parse still identifies the format: a truncated JSON line is
		// a jsonl line, and counting it as unrecognised would overstate how much
		// of the file nothing understood.
		if err != nil && !errors.Is(err, ErrPartial) {
			out[UnclaimedFormat]++
			continue
		}
		out[rec.Format]++
	}
	return out
}

// UnclaimedFormat is the key Formats uses for lines no format claimed.
const UnclaimedFormat = "(unrecognised)"
