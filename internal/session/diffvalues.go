package session

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The field and value half of a comparison. Templates are compared in diff.go;
// these are the other two kinds, and they share its statistic and its ranking
// so that a template, a field and a value are all measured on one scale.

// MaxDiffValues is how many distinct values a field may have before its values
// stop being compared one by one.
//
// A field whose values are nearly unique per record is an identifier, not a
// category, and "trace_id=a91c40f2 appeared" is true of every trace in the
// window — it is not a finding. Above this many, the field is still compared
// for presence and is named in the report as not compared by value, so the
// omission is visible rather than assumed.
const MaxDiffValues = 500

// diffColumns are the built-in columns worth comparing by value.
//
// Deliberately not every column. ts, seq and line_no are unique per record, raw
// and message are free text that internal/pattern already compares far better
// as templates, and pattern is its own kind here.
var diffColumns = []string{"level", "source", "file", "format", "parsed"}

// referenceable reports whether a field name can be written into a query.
//
// A control character cannot. The name goes into a JSON path literal inside a
// SQL string, and a NUL terminates that string at the database's C boundary, so
// the statement stops parsing partway through the name. This is not a
// comparison problem — it breaks any query that references such a field, and
// breaks ingest outright when the field is common enough to be promoted to a
// column — but a comparison is the only thing that references every field at
// once, so it is the only thing that meets it on ordinary data.
//
// Left out and named rather than allowed to fail the whole comparison. See
// EC022 in updates.md.
func referenceable(name string) bool {
	return !strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// diffFields is the fields whose presence and values are worth comparing, in a
// stable order.
func (s *Session) diffFields(ctx context.Context) ([]diffField, []string, error) {
	sch, err := s.Schema(ctx)
	if err != nil {
		return nil, nil, err
	}

	names := append([]string{}, diffColumns...)
	for name := range sch.Promoted {
		names = append(names, name)
	}
	names = append(names, sch.Fields...)

	seen := map[string]bool{}
	out := make([]diffField, 0, len(names))
	var unnameable []string

	for _, name := range names {
		if seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true

		if !referenceable(name) {
			unnameable = append(unnameable, strconv.Quote(name))
			continue
		}

		// A name that came from the schema resolves by construction; one that
		// did not is a column this build does not have, and skipping it beats
		// failing the whole comparison.
		expr, err := sch.Column(name)
		if err != nil {
			continue
		}
		out = append(out, diffField{Name: name, Expr: expr})
	}

	// Sorted so the comparison is deterministic before it is ranked, and so
	// the SQL below is byte-identical between runs over the same data.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	sort.Strings(unnameable)
	return out, unnameable, nil
}

// diffField is one field and the SQL that reads it.
type diffField struct {
	Name string
	Expr string
	// Distinct is how many values it takes across both windows, and Compare
	// whether that is few enough to compare value by value.
	Distinct int64
	Compare  bool
}

// diffValueFields decides which fields are worth comparing value by value.
func (s *Session) diffValueFields(ctx context.Context, fields []diffField, before, after Plan) ([]diffField, error) {
	if len(fields) == 0 {
		return nil, nil
	}

	selects := make([]string, len(fields))
	counts := make([]int64, len(fields))
	dest := make([]any, len(fields))
	for i, f := range fields {
		selects[i] = "count(DISTINCT " + f.Expr + ")"
		dest[i] = &counts[i]
	}

	where := "(" + before.SQL.Where + ") OR (" + after.SQL.Where + ")"
	args := append(append([]any{}, before.SQL.Args...), after.SQL.Args...)

	row := s.DB.QueryRow(ctx, "SELECT "+strings.Join(selects, ", ")+" FROM logs WHERE "+where, args...)
	if err := row.Scan(dest...); err != nil {
		return nil, fmt.Errorf("count the distinct values in each field: %w", err)
	}

	out := make([]diffField, len(fields))
	for i, f := range fields {
		f.Distinct = counts[i]
		f.Compare = counts[i] > 0 && counts[i] <= MaxDiffValues
		out[i] = f
	}
	return out, nil
}

// diffPresence counts, per field, how many records in one window carry it.
func (s *Session) diffPresence(ctx context.Context, fields []diffField, plan Plan) ([]int64, error) {
	if len(fields) == 0 {
		return nil, nil
	}

	selects := make([]string, len(fields))
	counts := make([]int64, len(fields))
	dest := make([]any, len(fields))
	for i, f := range fields {
		// count(expr) counts the records where it is not null, which is what
		// "carries this field" means everywhere else in the tool.
		selects[i] = "count(" + f.Expr + ")"
		dest[i] = &counts[i]
	}

	row := s.DB.QueryRow(ctx,
		"SELECT "+strings.Join(selects, ", ")+" FROM logs WHERE "+plan.SQL.Where,
		plan.SQL.Args...)
	if err := row.Scan(dest...); err != nil {
		return nil, fmt.Errorf("count the records carrying each field: %w", err)
	}
	return counts, nil
}

// diffValues counts every value of every comparable field in one window.
//
// One statement rather than one per field: the window is read once into a CTE
// and each field unpivoted off it, so the cost is a single scan however many
// fields there are.
//
// The field's own name is bound as a parameter rather than written into the
// statement. It came out of a log file, and a log file is not a trusted input —
// the JSON path inside the expression is escaped by internal/query for the same
// reason.
func (s *Session) diffValues(ctx context.Context, fields []diffField, plan Plan) (map[valueKey]int64, error) {
	arms := make([]string, 0, len(fields))
	args := append([]any{}, plan.SQL.Args...)

	for _, f := range fields {
		if !f.Compare {
			continue
		}
		args = append(args, f.Name)
		arms = append(arms, "SELECT ? AS field, CAST("+f.Expr+" AS VARCHAR) AS value, count(*) AS n"+
			" FROM w WHERE ("+f.Expr+") IS NOT NULL GROUP BY 1, 2")
	}
	if len(arms) == 0 {
		return map[valueKey]int64{}, nil
	}

	text := "WITH w AS (SELECT * FROM logs WHERE " + plan.SQL.Where + ") " +
		strings.Join(arms, " UNION ALL ")

	rows, err := s.DB.Query(ctx, text, args...)
	if err != nil {
		return nil, fmt.Errorf("count field values: %w", err)
	}
	defer rows.Close()

	out := map[valueKey]int64{}
	for rows.Next() {
		var (
			field, value string
			n            int64
		)
		if err := rows.Scan(&field, &value, &n); err != nil {
			return nil, fmt.Errorf("scan field value: %w", err)
		}
		out[valueKey{Field: field, Value: value}] = n
	}
	return out, rows.Err()
}

// valueKey identifies one value of one field.
type valueKey struct{ Field, Value string }

// diffFieldsAndValues compares which fields the records carry and, where a
// field has few enough values to be a category rather than an identifier, which
// values they take.
func (s *Session) diffFieldsAndValues(ctx context.Context, before, after Plan, set *DiffSet, compared map[DiffKind]int64) ([]DiffItem, error) {
	fields, unnameable, err := s.diffFields(ctx)
	if err != nil {
		return nil, err
	}
	set.Unnameable = unnameable

	fields, err = s.diffValueFields(ctx, fields, before, after)
	if err != nil {
		return nil, err
	}

	items, err := s.diffFieldItems(ctx, fields, before, after, set, compared)
	if err != nil {
		return nil, err
	}

	valueItems, err := s.diffValueItems(ctx, fields, before, after, compared)
	if err != nil {
		return nil, err
	}
	return append(items, valueItems...), nil
}

// diffFieldItems compares how many records in each window carry each field.
func (s *Session) diffFieldItems(ctx context.Context, fields []diffField, before, after Plan, set *DiffSet, compared map[DiffKind]int64) ([]DiffItem, error) {
	beforePresence, err := s.diffPresence(ctx, fields, before)
	if err != nil {
		return nil, err
	}
	afterPresence, err := s.diffPresence(ctx, fields, after)
	if err != nil {
		return nil, err
	}

	var items []DiffItem

	for i, f := range fields {
		if beforePresence[i] == 0 && afterPresence[i] == 0 {
			continue
		}
		compared[DiffField]++

		if !f.Compare && f.Distinct > 0 {
			set.Skipped = append(set.Skipped, DiffSkipped{Field: f.Name, Distinct: f.Distinct})
		}

		// A field every record carries needs no special case: its share is one
		// in both windows, so it scores zero and drops out with everything else
		// whose share did not move. One rule, applied to templates, fields and
		// values alike.
		items = append(items, DiffItem{
			Kind:   DiffField,
			Key:    f.Name,
			Label:  "field " + f.Name,
			Before: beforePresence[i],
			After:  afterPresence[i],
		})
	}
	return items, nil
}

// diffValueItems compares the values of the comparable fields.
func (s *Session) diffValueItems(ctx context.Context, fields []diffField, before, after Plan, compared map[DiffKind]int64) ([]DiffItem, error) {
	beforeValues, err := s.diffValues(ctx, fields, before)
	if err != nil {
		return nil, err
	}
	afterValues, err := s.diffValues(ctx, fields, after)
	if err != nil {
		return nil, err
	}

	keys := sortedValueKeys(beforeValues, afterValues)

	// A field with one value says nothing its own presence does not: the value
	// occurs on exactly the records that carry the field, so the two rows would
	// be identical. The field row is the one that survives, because it is the
	// one whose absence would be a finding.
	single := map[string]bool{}
	for _, f := range fields {
		single[f.Name] = f.Distinct == 1
	}

	items := make([]DiffItem, 0, len(keys))
	for _, key := range keys {
		if single[key.Field] {
			continue
		}
		items = append(items, DiffItem{
			Kind:   DiffValue,
			Key:    key.Field,
			Label:  key.Field + "=" + displayFieldValue(key.Value),
			Before: beforeValues[key],
			After:  afterValues[key],
		})
	}
	compared[DiffValue] = int64(len(keys))
	return items, nil
}

// displayFieldValue names a value that is present but empty.
//
// A blank where a value should be reads as a rendering fault rather than as the
// data, which is the same call EC005 made for `loupe top`.
func displayFieldValue(v string) string {
	if v == "" {
		return "(empty)"
	}
	return v
}

// sortedValueKeys is every (field, value) pair either window holds, in a
// deterministic order.
//
// Sorted because map iteration order is not, and the ranking that follows is
// stable: a report that reordered itself between runs could not be compared
// against one taken a minute earlier.
func sortedValueKeys(sides ...map[valueKey]int64) []valueKey {
	seen := map[valueKey]bool{}
	var keys []valueKey

	for _, side := range sides {
		for key := range side {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}

	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Field != keys[j].Field {
			return keys[i].Field < keys[j].Field
		}
		return keys[i].Value < keys[j].Value
	})
	return keys
}
