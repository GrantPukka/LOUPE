package session

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/GrantPukka/loupe/internal/query"
)

// DefaultFieldLimit is how many fields a listing shows when the caller does not
// say.
//
// Generous, because this is the command someone runs precisely because they do
// not know what is there, and a truncated answer to "what is there" is a poor
// one. What the limit cuts is counted and stated.
const DefaultFieldLimit = 50

// fieldExamples is how many example values each field shows.
const fieldExamples = 3

// builtinFields are the columns every record has, in the order they are worth
// reading.
//
// Written out rather than taken from Schema.Known, which also lists the aliases
// — msg, line, pattern — and a table with both message and msg in it answers a
// question nobody asked. The aliases still work in a filter; the footer says so.
var builtinFields = []string{
	"ts", "ts_zoned", "level", "message", "source", "file", "format",
	"line_no", "parsed", "pattern", "raw", "seq",
}

// FieldInfo is one field and everything worth knowing before filtering on it.
type FieldInfo struct {
	Name string `json:"name"`

	// Records is how many matching records carry the field, and Coverage that
	// as a fraction of the ones the filter matched.
	Records  int64   `json:"records"`
	Coverage float64 `json:"coverage"`

	// Distinct is how many different values it takes. It is the number that
	// says whether `loupe top` on this field will be a distribution or a list
	// of identifiers.
	Distinct int64 `json:"distinct"`

	// Type is the field's dominant type in the words the tool uses elsewhere,
	// and Types every type it was seen holding.
	//
	// More than one is worth surfacing rather than averaging away: a
	// latency_ms that is a number on most records and a string on the rest
	// will silently drop the strings from latency_ms:>1000, which is exactly
	// the confident wrong answer this project exists to avoid.
	Type  string   `json:"type"`
	Types []string `json:"types,omitempty"`

	// Numeric is how many of the field's values can be read as a number.
	//
	// Between one and all of them is the case worth knowing about: an ordering
	// comparison casts, and a value that does not cast is skipped rather than
	// reported. See PartlyNumeric.
	Numeric int64 `json:"numeric"`

	// Column reports that the field is read from a real column rather than
	// extracted from the JSON bag on every row.
	Column bool `json:"column"`

	// Examples are common values, for recognising the shape of the field.
	Examples []string `json:"examples,omitempty"`
}

// Mixed reports whether the field was seen holding more than one type.
func (f FieldInfo) Mixed() bool { return len(f.Types) > 1 }

// PartlyNumeric reports a field most of whose values are numbers, but not all.
//
// That is the shape somebody will reach for an ordering comparison on, and
// latency_ms:>1000 casts — so the handful of values that are not numbers are
// skipped without a word. Saying so before the filter is written is the whole
// point of this command.
//
// A majority rather than any: three log messages that happen to be bare numbers
// do not make `message` a numeric field, and a note on every text column would
// be noise where the real one has to stand out.
func (f FieldInfo) PartlyNumeric() bool {
	return f.Numeric > 0 && f.Numeric < f.Records && f.Numeric*2 > f.Records
}

// FieldQuery configures a listing.
type FieldQuery struct {
	// Limit is how many fields to return, best covered first. Zero uses
	// DefaultFieldLimit; a negative limit means all of them.
	Limit int
}

// FieldSet is a listing plus everything needed to trust it.
type FieldSet struct {
	Fields []FieldInfo `json:"fields"`

	// Matched is how many records the filter matched, which is the
	// denominator every coverage figure is of.
	Matched int64 `json:"matched"`

	// Known is how many fields exist in this data at all, and Absent how many
	// of those no matching record carries.
	//
	// The gap is the answer to "is this field missing from my results, or
	// missing from the data" — two very different things to be looking at.
	Known  int64 `json:"known"`
	Absent int64 `json:"absent"`

	// Unnameable names the fields left out because their names cannot be
	// written into a query. See EC022.
	Unnameable []string `json:"unnameable,omitempty"`

	Truncated bool  `json:"truncated"`
	Hidden    int64 `json:"hidden,omitempty"`
}

// Fields reports what can be filtered on, and what the values look like.
//
// Today a field is discovered by getting one wrong: a typo'd name comes back
// with the list attached. That works and is the wrong way round — this is the
// command that answers the question directly, before the mistake.
func (s *Session) Fields(ctx context.Context, plan Plan, q FieldQuery) (FieldSet, error) {
	candidates, unnameable, err := s.fieldCandidates(ctx)
	if err != nil {
		return FieldSet{}, err
	}

	out := FieldSet{Unnameable: unnameable, Known: int64(len(candidates))}
	if err := s.DB.QueryRow(ctx,
		"SELECT count(*) FROM logs WHERE "+plan.SQL.Where, plan.SQL.Args...).
		Scan(&out.Matched); err != nil {
		return FieldSet{}, fmt.Errorf("count matching records: %w", err)
	}

	infos, err := s.fieldStats(ctx, candidates, plan)
	if err != nil {
		return FieldSet{}, err
	}

	// A field no matching record carries is counted rather than listed. Its
	// row would be a line of zeroes, and thirty of those bury the fields the
	// question was about.
	for _, info := range infos {
		if info.Records == 0 {
			out.Absent++
			continue
		}
		info.Coverage = float64(info.Records) / float64(out.Matched)
		out.Fields = append(out.Fields, info)
	}

	sortFields(out.Fields)

	limit := q.Limit
	if limit == 0 {
		limit = DefaultFieldLimit
	}
	if limit > 0 && len(out.Fields) > limit {
		out.Hidden = int64(len(out.Fields) - limit)
		out.Truncated = true
		out.Fields = out.Fields[:limit]
	}

	return out, nil
}

// sortFields orders by coverage, best covered first.
//
// Frequency rather than alphabet, which is the order `loupe top` and `loupe
// patterns` both use: the fields on every record are the ones a filter is most
// likely to want, and a rare field near the bottom is itself a useful signal.
// Name breaks ties so the same data always lists in the same order.
func sortFields(fields []FieldInfo) {
	sort.SliceStable(fields, func(i, j int) bool {
		if fields[i].Records != fields[j].Records {
			return fields[i].Records > fields[j].Records
		}
		return fields[i].Name < fields[j].Name
	})
}

// fieldCandidate is one field and how to read it.
type fieldCandidate struct {
	name string
	expr string
	// column is true when the field has a real column, and jsonPath the path
	// literal for the ones that do not.
	column   bool
	jsonPath string
}

// fieldCandidates is every field a filter could name, with the SQL that reads
// it.
func (s *Session) fieldCandidates(ctx context.Context) ([]fieldCandidate, []string, error) {
	sch, err := s.Schema(ctx)
	if err != nil {
		return nil, nil, err
	}

	names := append([]string{}, builtinFields...)
	for name := range sch.Promoted {
		names = append(names, name)
	}
	names = append(names, sch.Fields...)

	seen := map[string]bool{}
	out := make([]fieldCandidate, 0, len(names))
	var unnameable []string

	for _, name := range names {
		if seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true

		// A control character in a name cannot be written into a JSON path
		// literal; see EC022. Left out and named, rather than failing the
		// listing that exists to say what is there.
		if !referenceable(name) {
			unnameable = append(unnameable, strconv.Quote(name))
			continue
		}

		expr, err := sch.Column(name)
		if err != nil {
			continue
		}

		c := fieldCandidate{name: name, expr: expr}
		// A promoted or built-in field resolves to a bare column reference; a
		// bag field resolves to an extraction, which is also the shape whose
		// stored type has to be asked for separately.
		if path, isBag := query.BagPath(expr); isBag {
			c.jsonPath = path
		} else {
			c.column = true
		}
		out = append(out, c)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	sort.Strings(unnameable)
	return out, unnameable, nil
}

// fieldStats counts, types and samples every candidate in one pass.
//
// One statement rather than one per field: this is a discovery command run on a
// directory nobody has looked at yet, and thirty separate scans of a 10M-row
// table to answer "what is here" would make the answer not worth waiting for.
func (s *Session) fieldStats(ctx context.Context, candidates []fieldCandidate, plan Plan) ([]FieldInfo, error) {
	if len(candidates) == 0 {
		return nil, nil
	}

	columnTypes, err := s.columnTypes(ctx)
	if err != nil {
		return nil, err
	}

	var selects []string
	for _, c := range candidates {
		selects = append(selects,
			"count("+c.expr+")",
			"count(DISTINCT "+c.expr+")",
			// The same cast an ordering comparison makes, so the count says
			// exactly how many values such a filter would keep.
			"count(TRY_CAST("+c.expr+" AS DOUBLE))",
			// approx_top_k is approximate by design and that is the right
			// trade here: these are examples of what the values look like, not
			// a ranking, and an exact top-k would mean a GROUP BY per field.
			"coalesce(array_to_string(approx_top_k(CAST("+c.expr+" AS VARCHAR), "+
				strconv.Itoa(fieldExamples)+"), '\x1f'), '')")

		if c.column {
			continue
		}
		// The stored type of a bag value, and how many types it was seen
		// holding. A real column has one type by construction.
		selects = append(selects,
			"coalesce(mode(json_type(fields, "+c.jsonPath+")), '')",
			"count(DISTINCT json_type(fields, "+c.jsonPath+"))")
	}

	infos := make([]FieldInfo, len(candidates))
	examples := make([]string, len(candidates))
	types := make([]string, len(candidates))
	typeCounts := make([]int64, len(candidates))

	dest := make([]any, 0, len(selects))
	for i, c := range candidates {
		infos[i].Name, infos[i].Column = c.name, c.column

		dest = append(dest, &infos[i].Records, &infos[i].Distinct, &infos[i].Numeric, &examples[i])
		if !c.column {
			dest = append(dest, &types[i], &typeCounts[i])
		}
	}

	row := s.DB.QueryRow(ctx,
		"SELECT "+strings.Join(selects, ", ")+" FROM logs WHERE "+plan.SQL.Where,
		plan.SQL.Args...)
	if err := row.Scan(dest...); err != nil {
		return nil, fmt.Errorf("describe the fields: %w", err)
	}

	for i, c := range candidates {
		infos[i].Examples = splitExamples(examples[i])
		if c.column {
			infos[i].Type = friendlyType(columnTypes[strings.Trim(c.expr, `"`)])
			continue
		}
		infos[i].Type = friendlyType(types[i])
		if typeCounts[i] > 1 {
			infos[i].Types = []string{infos[i].Type}
		}
	}

	return s.fieldTypeLists(ctx, candidates, infos, plan)
}

// fieldTypeLists fills in the full type list for the fields that hold more than
// one.
//
// A second query, run only when the first found a mixed field, because that is
// rare and the list is worth naming exactly rather than approximating. Nothing
// runs at all on the common case where every field is consistently typed.
func (s *Session) fieldTypeLists(ctx context.Context, candidates []fieldCandidate, infos []FieldInfo, plan Plan) ([]FieldInfo, error) {
	var mixed []int
	for i := range infos {
		if len(infos[i].Types) > 0 {
			mixed = append(mixed, i)
		}
	}
	if len(mixed) == 0 {
		return infos, nil
	}

	var selects []string
	for _, i := range mixed {
		selects = append(selects,
			"array_to_string(list_sort(list(DISTINCT json_type(fields, "+
				candidates[i].jsonPath+"))), '\x1f')")
	}

	lists := make([]string, len(mixed))
	dest := make([]any, len(mixed))
	for i := range lists {
		dest[i] = &lists[i]
	}

	row := s.DB.QueryRow(ctx,
		"SELECT "+strings.Join(selects, ", ")+" FROM logs WHERE "+plan.SQL.Where,
		plan.SQL.Args...)
	if err := row.Scan(dest...); err != nil {
		return nil, fmt.Errorf("list the types of the mixed fields: %w", err)
	}

	for n, i := range mixed {
		infos[i].Types = nil
		for _, t := range strings.Split(lists[n], "\x1f") {
			if name := friendlyType(t); name != "" {
				infos[i].Types = append(infos[i].Types, name)
			}
		}
	}
	return infos, nil
}

// columnTypes reads the real columns' declared types.
//
// One DESCRIBE rather than a typeof() aggregate per column: a column's type is
// a property of the table, not of the rows a filter matched.
func (s *Session) columnTypes(ctx context.Context) (map[string]string, error) {
	rows, err := s.DB.Query(ctx, "DESCRIBE logs")
	if err != nil {
		return nil, fmt.Errorf("describe logs: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("describe logs: %w", err)
	}

	out := map[string]string{}
	for rows.Next() {
		scan := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range scan {
			ptrs[i] = &scan[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("scan column description: %w", err)
		}
		name, _ := scan[0].(string)
		kind, _ := scan[1].(string)
		out[name] = kind
	}
	return out, rows.Err()
}

// splitExamples unpacks the aggregated example values.
//
// A unit separator rather than a comma, for the reason internal/session's
// pattern listing already found: a value comes out of a log file and nothing
// stops one containing a comma. Splitting on a character the data cannot hold
// is the difference between a list and a guess.
func splitExamples(joined string) []string {
	if joined == "" {
		return nil
	}
	return strings.Split(joined, "\x1f")
}

// friendlyType renders a DuckDB or JSON type in the words the rest of the tool
// uses. A user filtering on a field does not care that an integer arrived as a
// UBIGINT.
func friendlyType(name string) string {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "":
		return ""
	case "VARCHAR", "TEXT", "STRING":
		return "string"
	case "BIGINT", "UBIGINT", "INTEGER", "UINTEGER", "HUGEINT", "SMALLINT", "TINYINT":
		return "integer"
	case "DOUBLE", "FLOAT", "REAL", "DECIMAL":
		return "float"
	case "BOOLEAN":
		return "boolean"
	case "TIMESTAMP", "TIMESTAMP WITH TIME ZONE", "DATE", "TIME":
		return "timestamp"
	case "JSON", "OBJECT", "STRUCT":
		return "object"
	case "ARRAY", "LIST":
		return "array"
	case "NULL":
		return "null"
	default:
		return strings.ToLower(name)
	}
}
