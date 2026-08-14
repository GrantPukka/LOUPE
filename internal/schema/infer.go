package schema

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Kind is the DuckDB type a promoted field gets.
type Kind int

const (
	// KindString is the fallback. Anything ambiguous ends up here, because a
	// wrong narrow type turns values into NULL and loses them.
	KindString Kind = iota
	KindInt
	KindFloat
	KindBool
	KindTimestamp
)

// SQLType is the DuckDB type name.
func (k Kind) SQLType() string {
	switch k {
	case KindInt:
		return "BIGINT"
	case KindFloat:
		return "DOUBLE"
	case KindBool:
		return "BOOLEAN"
	case KindTimestamp:
		return "TIMESTAMP"
	default:
		return "VARCHAR"
	}
}

func (k Kind) String() string { return k.SQLType() }

// Sample is one record's field bag together with the source it came from.
//
// The source matters: coverage is judged per source, not across the whole
// directory. See Infer.
type Sample struct {
	Source string
	Fields map[string]any
}

// Promotion is one field that earned a real column.
type Promotion struct {
	// Field is the key as it appears in the fields bag.
	Field string `json:"field"`
	// Column is the SQL identifier it becomes.
	Column string `json:"column"`
	Kind   Kind   `json:"kind"`
	// Present is how many sampled records carried the field.
	Present int `json:"present"`
	// Coverage is the fraction of all sampled records carrying it.
	Coverage float64 `json:"coverage"`
	// BestCoverage is the fraction within the source that uses it most, and
	// is what the promotion decision is actually made on.
	BestCoverage float64 `json:"best_coverage"`
	// BestSource names that source.
	BestSource string `json:"best_source"`
}

// Skip records a field that was considered and passed over, with the reason.
//
// These are reported rather than discarded: a user wondering why status is slow
// to filter deserves to know it collided with a reserved name or fell below the
// coverage threshold.
type Skip struct {
	Field  string
	Reason string
}

// Options tunes inference. The zero value uses the defaults from
// ARCHITECTURE.md section 3.3.
type Options struct {
	// SampleSize is how many records are examined. Defaults to 10,000.
	SampleSize int
	// MinCoverage is the fraction of sampled records a field must appear in.
	// Defaults to 0.6.
	MinCoverage float64
	// MaxColumns caps the promoted set. A directory with 500 distinct keys
	// should not produce a 500-column table.
	MaxColumns int
}

const (
	defaultSampleSize  = 10_000
	defaultMinCoverage = 0.6
	defaultMaxColumns  = 32
)

// WithDefaults fills in the unset fields.
//
// Callers outside this package need the resolved sample size to build their
// query, so the defaults cannot stay private to Infer.
func (o Options) WithDefaults() Options {
	return Options{
		SampleSize:  o.sampleSize(),
		MinCoverage: o.minCoverage(),
		MaxColumns:  o.maxColumns(),
	}
}

func (o Options) sampleSize() int {
	if o.SampleSize > 0 {
		return o.SampleSize
	}
	return defaultSampleSize
}

func (o Options) minCoverage() float64 {
	if o.MinCoverage > 0 {
		return o.MinCoverage
	}
	return defaultMinCoverage
}

func (o Options) maxColumns() int {
	if o.MaxColumns > 0 {
		return o.MaxColumns
	}
	return defaultMaxColumns
}

// Reserved are the fixed columns a promoted field must not collide with.
var Reserved = map[string]bool{
	"seq": true, "ts": true, "ts_zoned": true, "level": true, "message": true,
	"source": true, "file": true, "format": true, "line_no": true,
	"parsed": true, "raw": true, "fields": true,
}

// observation accumulates what was seen for one field.
type observation struct {
	present int
	// perSource counts how many records of each source carried the field.
	perSource map[string]int

	ints       int
	floats     int
	bools      int
	strings    int
	timestamps int
	// nested counts objects and arrays, which are never promoted.
	nested int
	// nulls are not evidence of a type either way.
	nulls int
}

// Infer decides which fields deserve real columns.
//
// ARCHITECTURE.md section 3.3 sets the rule as a field appearing in more than
// 60% of sampled records. That threshold assumes one coherent log stream, and
// applied across a directory of unlike sources it promotes almost nothing:
// measured on the demo directory, the most common field of all reaches 59.9%,
// because six formats each contribute their own vocabulary.
//
// So coverage is judged **within a source**. A field carried by every Nginx
// record is a good column even though Postgres never sets it — that is what
// NULL is for, and the alternative is a JSON extraction on every row forever.
// Both figures are recorded so the decision can be explained.
//
// The harder half is the type, where the governing principle is that widening
// is safe and narrowing is not. A wrong narrow type does not error — TRY_CAST
// turns the value into NULL — so it silently deletes data, which is the failure
// this project exists to avoid.
func Infer(samples []Sample, opts Options) ([]Promotion, []Skip) {
	if len(samples) == 0 {
		return nil, nil
	}

	observed := map[string]*observation{}
	sourceTotals := map[string]int{}

	for _, sample := range samples {
		sourceTotals[sample.Source]++

		for key, value := range sample.Fields {
			o := observed[key]
			if o == nil {
				o = &observation{perSource: map[string]int{}}
				observed[key] = o
			}
			o.present++
			o.perSource[sample.Source]++
			o.record(value)
		}
	}

	// Sort by coverage, then name, so the MaxColumns cut is deterministic and
	// takes the most useful fields.
	keys := make([]string, 0, len(observed))
	for key := range observed {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := observed[keys[i]], observed[keys[j]]
		if a.present != b.present {
			return a.present > b.present
		}
		return keys[i] < keys[j]
	})

	var (
		promotions []Promotion
		skips      []Skip
		taken      = map[string]bool{}
	)

	for _, key := range keys {
		o := observed[key]
		coverage := float64(o.present) / float64(len(samples))
		bestCoverage, bestSource := o.best(sourceTotals)

		if bestCoverage < opts.minCoverage() {
			skips = append(skips, Skip{key, fmt.Sprintf(
				"reaches only %.0f%% of records in its most frequent source (%s), below the %.0f%% threshold",
				bestCoverage*100, bestSource, opts.minCoverage()*100)})
			continue
		}
		if o.nested > 0 {
			skips = append(skips, Skip{key, "holds objects or arrays"})
			continue
		}

		column := columnName(key)
		switch {
		case column == "":
			skips = append(skips, Skip{key, "name has no usable characters for a column"})
			continue
		case Reserved[column]:
			skips = append(skips, Skip{key, "name collides with a built-in column"})
			continue
		case taken[column]:
			skips = append(skips, Skip{key, "name collides with another promoted field"})
			continue
		}

		if len(promotions) >= opts.maxColumns() {
			skips = append(skips, Skip{key, fmt.Sprintf(
				"more than %d fields qualified; this one is below the cut", opts.maxColumns())})
			continue
		}

		taken[column] = true
		promotions = append(promotions, Promotion{
			Field:        key,
			Column:       column,
			Kind:         o.kind(),
			Present:      o.present,
			Coverage:     coverage,
			BestCoverage: bestCoverage,
			BestSource:   bestSource,
		})
	}

	return promotions, skips
}

// best returns the field's highest coverage within any single source, and
// which source that was.
func (o *observation) best(sourceTotals map[string]int) (float64, string) {
	var (
		bestCoverage float64
		bestSource   string
	)

	for source, count := range o.perSource {
		total := sourceTotals[source]
		if total == 0 {
			continue
		}
		coverage := float64(count) / float64(total)
		// Ties break by source name so the reported source is stable across
		// runs; the promotion decision is cached and must not wobble.
		if coverage > bestCoverage || (coverage == bestCoverage && source < bestSource) {
			bestCoverage, bestSource = coverage, source
		}
	}

	return bestCoverage, bestSource
}

// record notes one value's type.
func (o *observation) record(value any) {
	switch v := value.(type) {
	case nil:
		o.nulls++
	case bool:
		o.bools++
	case int64:
		o.ints++
	case int:
		o.ints++
	case float64:
		// A JSON number with no fractional part decodes as int64 through the
		// parsers, so a float here is genuinely fractional.
		o.floats++
	case string:
		if looksLikeTimestamp(v) {
			o.timestamps++
			return
		}
		o.strings++
	case time.Time:
		o.timestamps++
	case map[string]any, []any:
		o.nested++
	default:
		o.strings++
	}
}

// kind resolves the observations into one type.
//
// Any disagreement widens to VARCHAR. A field whose type changes halfway
// through a file is one of the traps ARCHITECTURE.md calls out, and the honest
// answer is the type that can hold both.
func (o *observation) kind() Kind {
	typed := o.ints + o.floats + o.bools + o.strings + o.timestamps
	if typed == 0 {
		return KindString
	}

	switch {
	case o.strings > 0:
		// A single non-numeric string is enough. Note that a numeric-looking
		// string never reaches here as an int: the parsers already decide
		// that, and logfmt deliberately keeps 007 a string because an id with
		// a leading zero is not the number seven.
		return KindString

	case o.timestamps == typed:
		return KindTimestamp
	case o.timestamps > 0:
		// Timestamps mixed with numbers or booleans have nothing in common but
		// text.
		return KindString

	case o.bools == typed:
		return KindBool
	case o.bools > 0:
		return KindString

	case o.floats > 0:
		// Integers widen into floats losslessly for the magnitudes logs carry.
		return KindFloat
	default:
		return KindInt
	}
}

// looksLikeTimestamp reports whether a string is an unambiguous date-time.
//
// Only offset-bearing and ISO-8601 shapes count. A bare integer is deliberately
// never treated as epoch millis here: in a log it is far more often an id, a
// port, or a byte count, and turning one into a date in 1970 or 2033 would be a
// confident wrong answer. That matches the conservatism in parse.parseEpoch.
func looksLikeTimestamp(s string) bool {
	if len(s) < 10 || len(s) > 40 {
		return false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if _, err := time.Parse(layout, s); err == nil {
			return true
		}
	}
	return false
}

// columnName turns a field key into a safe SQL identifier.
//
// Log field names contain dots, dashes, and occasionally spaces. Lowercasing
// keeps them consistent with the built-in columns, which the DSL already
// matches case-insensitively.
func columnName(field string) string {
	var sb strings.Builder
	sb.Grow(len(field))

	for _, r := range strings.ToLower(field) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			sb.WriteRune(r)
		case r == '_' || r == '.' || r == '-' || r == ' ' || r == '/':
			sb.WriteByte('_')
		}
	}

	out := strings.Trim(sb.String(), "_")
	if out == "" {
		return ""
	}
	// An identifier cannot start with a digit.
	if out[0] >= '0' && out[0] <= '9' {
		out = "f_" + out
	}
	return out
}
