package parse

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ErrNoMatch means this parser cannot handle this line. It is an ordinary,
// expected outcome, not a failure: the caller keeps the raw text and marks the
// record unparsed.
//
// A parser must never return a fatal error for one bad line. One corrupt line
// must not abort a 4GB file.
var ErrNoMatch = errors.New("line does not match this format")

// Record is one log entry, normalised across every format.
type Record struct {
	// Timestamp is the zero value when the line carried none. That is not an
	// error: the record is still ingested and still queryable via ts:none.
	Timestamp time.Time

	// TimestampZoned reports whether the line carried its own timezone. When
	// false, Timestamp was resolved using the source's assumed zone, and
	// docs/FILTER-DSL.md section 2.5 requires that assumption be disclosed
	// rather than hidden.
	//
	// False is the safe default: an undisclosed assumption is the failure mode
	// worth designing against.
	TimestampZoned bool

	// Level is normalised to trace/debug/info/warn/error/fatal by
	// NormaliseLevel, so that level:>=warn works across formats. A level
	// outside that set keeps its original string.
	Level string

	Message string

	// Fields holds everything else. Never drop a key: anything dropped here is
	// invisible forever.
	Fields map[string]any

	// Format names the parser that produced this record, and is set only when
	// that differs per record — which is to say, only by the mixed parser. Left
	// empty, the record takes its source's format, which is the answer for
	// every file that really is one format.
	//
	// It is a field rather than a method on Parser because it is a fact about
	// one record, not a capability a parser author has to implement.
	Format string
}

// HasTimestamp reports whether a timestamp was extracted.
func (r Record) HasTimestamp() bool { return !r.Timestamp.IsZero() }

// Parser converts one log format's bytes into Records.
//
// This interface is the project's contribution surface. Keep it tiny: do not
// add a method to solve a problem local to one format. See CONTRIBUTING.md.
type Parser interface {
	// Name is the format identifier used by --parser and by format: filters.
	Name() string

	// Detect returns 0.0-1.0 confidence given a sample of lines from the head
	// of a source. Be conservative: returning 0.3 and losing a coin flip is
	// much better than returning 0.9 and hijacking another parser's format.
	Detect(sample [][]byte) float64

	// Parse converts one logical line into a Record. It returns ErrNoMatch for
	// a line it cannot handle.
	Parse(line []byte) (Record, error)
}

// Continuer is implemented by parsers whose records can span multiple physical
// lines, such as Log4j with Java stack traces.
//
// It is deliberately a separate optional interface rather than a method on
// Parser: most formats have no continuation concept, and every parser author
// should not have to think about one.
type Continuer interface {
	// IsContinuation reports whether line continues the preceding record
	// rather than starting a new one.
	IsContinuation(line []byte) bool
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Parser{}
)

// Register adds a parser to the registry. Parsers call it from init(), so
// adding a format touches exactly one file.
//
// It panics on a duplicate name, which can only be a programming error and is
// better caught at startup than by a silently shadowed parser.
func Register(p Parser) {
	registryMu.Lock()
	defer registryMu.Unlock()

	name := p.Name()
	if name == "" {
		panic("parse: parser registered with an empty name")
	}
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("parse: duplicate parser name %q", name))
	}
	registry[name] = p
}

// Get returns the parser with the given name.
func Get(name string) (Parser, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	p, ok := registry[name]
	return p, ok
}

// Names lists every registered parser, sorted, for help text and error
// messages.
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()

	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// All returns every registered parser, ordered by name so that detection is
// deterministic when two parsers tie.
func All() []Parser {
	registryMu.RLock()
	defer registryMu.RUnlock()

	out := make([]Parser, 0, len(registry))
	for _, name := range sortedNames() {
		out = append(out, registry[name])
	}
	return out
}

// sortedNames must be called with registryMu held.
func sortedNames() []string {
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
