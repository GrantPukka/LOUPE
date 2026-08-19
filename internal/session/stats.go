package session

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/GrantPukka/loupe/internal/query"
	"github.com/GrantPukka/loupe/internal/store"
)

// IsAggregate reports whether a filter carries an aggregation clause.
//
// Parsing is pure and needs no data, so a caller can ask before it opens
// anything. A filter that does not parse is not an aggregation: the real error
// comes back with the data's own field names attached when the session plans
// it, which is a better error than anything this could raise here.
func IsAggregate(filter string) bool {
	q, err := query.Parse(filter)
	return err == nil && q.Stats != nil
}

// StatsQuery configures running an aggregation.
type StatsQuery struct {
	// Limit is how many rows to return. Zero means no limit; truncation is
	// always declared by the result.
	Limit int
}

// StatsSet is an aggregation and everything needed to trust it.
type StatsSet struct {
	// Result is the table itself: grouping columns first, then aggregates.
	Result store.Result

	// Clause is the aggregation as it was resolved, rendered back to DSL text.
	Clause string

	// Matched is how many records the filter matched, and Grouped how many of
	// those the aggregation could place. The two differ when a grouping field
	// is missing from some records, or when a bin meets a record with no
	// timestamp.
	Matched int64
	Grouped int64

	// Absent is, per grouping field, how many matched records carry no value
	// for it and so belong to no group.
	//
	// Reported rather than folded in. A breakdown by path that quietly drops
	// the 88 records with no path is a breakdown of something other than what
	// the filter matched, and the reader has no way to tell.
	Absent []StatsAbsent

	// NoTimestamp is how many matched records carry no timestamp and so fall
	// in no time bucket. Only set when the grouping has a bin.
	NoTimestamp int64

	// EmptyBins is how many buckets between the first and the last hold no
	// matching record.
	//
	// A bucket with nothing in it is not a group, so it has no row — which
	// means a rate table can put 14:04 directly above 14:06 and read as
	// continuous when the middle minute was silent. Counting them is cheaper
	// than listing them and says the same thing.
	EmptyBins int64

	// Bin and Origin describe the time bucketing, zero when there is none.
	Bin    time.Duration
	Origin time.Time

	// Notes are things the reader must be told about the numbers: values a
	// numeric aggregate could not read, a field nothing carries, a clock
	// change that moves the bucket boundaries.
	Notes []string
}

// StatsAbsent is one grouping field and the records that lack it.
type StatsAbsent struct {
	Field string `json:"field"`
	Count int64  `json:"count"`
}

// Stats runs the query's aggregation clause.
//
// This is the fast lane for the questions people otherwise drop to `loupe sql`
// for — counts, rates and p99s — and it goes through the same parsed AST and
// the same parameterised compilation as every filter, so a stats query cannot
// mean something different from the filter beside it.
func (s *Session) Stats(ctx context.Context, plan Plan, q StatsQuery) (StatsSet, error) {
	stats := plan.Query.Stats
	if stats == nil {
		return StatsSet{}, fmt.Errorf("this query has no stats clause, e.g. `stats count() by level`")
	}

	sch, err := s.Schema(ctx)
	if err != nil {
		return StatsSet{}, err
	}

	opts := query.StatsOptions{}
	out := StatsSet{Clause: stats.String()}

	if stats.HasBin() {
		origin, note, err := s.binAnchor(ctx, plan, stats.BinWidth())
		if err != nil {
			return StatsSet{}, err
		}
		opts.Origin = origin
		if note != "" {
			out.Notes = append(out.Notes, note)
		}
	}

	compiled, err := query.CompileStats(stats, sch, opts)
	if err != nil {
		return StatsSet{}, err
	}
	out.Bin, out.Origin = compiled.Bin, compiled.Origin

	where, args := statsWhere(plan, compiled)

	if err := s.statsPopulation(ctx, plan, compiled, &out); err != nil {
		return StatsSet{}, err
	}
	notes, err := s.statsNumeric(ctx, compiled, where, args)
	if err != nil {
		return StatsSet{}, err
	}
	out.Notes = append(out.Notes, notes...)

	res, err := s.DB.QueryResult(ctx, q.Limit, statsSQL(compiled, where), args...)
	if err != nil {
		return StatsSet{}, fmt.Errorf("run %s: %w", out.Clause, err)
	}
	out.Result = res

	return out, nil
}

// statsWhere ANDs the filter with the conditions a grouping needs.
//
// A record with no value for a grouping column belongs to no group, so it
// cannot appear in the table. Excluding it here rather than letting it collect
// in a nameless row is what makes the counts in the footer possible: the reader
// is told how many were left out and how to find them, instead of being shown
// a blank cell that reads as a rendering fault.
func statsWhere(plan Plan, compiled query.StatsSQL) (string, []any) {
	where := plan.SQL.Where
	if groupable := statsGroupable(compiled); groupable != "" {
		where = "(" + where + ") AND " + groupable
	}
	return where, plan.SQL.Args
}

// statsGroupable is the condition a record must meet to belong to a group at
// all, across every grouping column.
//
// It carries no parameters — a grouping column is a field name resolved by the
// schema, never a value from the query string — which is what lets the counting
// query reuse it inside a FILTER without binding the filter's arguments twice.
func statsGroupable(compiled query.StatsSQL) string {
	var parts []string
	for _, col := range compiled.Select {
		if col.Present != "" {
			parts = append(parts, col.Present)
		}
	}
	return strings.Join(parts, " AND ")
}

// statsSQL assembles the aggregation.
func statsSQL(compiled query.StatsSQL, where string) string {
	items := make([]string, len(compiled.Select))
	for i, col := range compiled.Select {
		items[i] = col.SelectItem()
	}

	sqlText := "SELECT " + strings.Join(items, ", ") + " FROM logs WHERE " + where
	if compiled.GroupBy != "" {
		sqlText += " " + compiled.GroupBy
	}
	if compiled.OrderBy != "" {
		sqlText += " " + compiled.OrderBy
	}
	return sqlText
}

// statsPopulation counts what the filter matched, what the aggregation could
// place, and what each grouping field left behind.
func (s *Session) statsPopulation(ctx context.Context, plan Plan, compiled query.StatsSQL, out *StatsSet) error {
	groupable := statsGroupable(compiled)
	grouped := "count(*)"
	if groupable != "" {
		grouped = "count(*) FILTER (WHERE " + groupable + ")"
	}

	var (
		selects = []string{"count(*)", grouped}
		fields  []string
		bin     query.StatsColumn
	)

	for _, col := range compiled.Select {
		if !col.Group {
			continue
		}
		if col.Bin {
			bin = col
			continue
		}
		selects = append(selects, "count(*) FILTER (WHERE NOT ("+col.Present+"))")
		fields = append(fields, col.Name)
	}
	if bin.Expr != "" {
		selects = append(selects, "count(*) FILTER (WHERE ts IS NULL)")
	}

	counts := make([]int64, len(selects))
	dest := make([]any, len(selects))
	for i := range dest {
		dest[i] = &counts[i]
	}

	// The bin extent goes on the same scan: it is what says whether the table
	// has holes in it.
	var first, last sql.NullTime
	var distinct int64
	if bin.Expr != "" {
		dest = append(dest, &distinct, &first, &last)
		selects = append(selects,
			"count(DISTINCT "+bin.Expr+") FILTER (WHERE "+groupable+")",
			"min("+bin.Expr+") FILTER (WHERE "+groupable+")",
			"max("+bin.Expr+") FILTER (WHERE "+groupable+")")
	}

	// The population is counted against the filter alone, so the exclusions
	// are measured against everything the user asked for.
	row := s.DB.QueryRow(ctx,
		"SELECT "+strings.Join(selects, ", ")+" FROM logs WHERE "+plan.SQL.Where,
		plan.SQL.Args...)
	if err := row.Scan(dest...); err != nil {
		return fmt.Errorf("count the records behind %s: %w", out.Clause, err)
	}

	out.Matched, out.Grouped = counts[0], counts[1]
	for i, field := range fields {
		if counts[2+i] > 0 {
			out.Absent = append(out.Absent, StatsAbsent{Field: field, Count: counts[2+i]})
		}
	}
	if bin.Expr != "" {
		out.NoTimestamp = counts[len(counts)-1]
		out.EmptyBins = emptyBins(first, last, distinct, compiled.Bin)
	}
	return nil
}

// emptyBins counts the buckets between the first and the last that hold
// nothing.
//
// Only the span the data actually covers: buckets outside it were never part
// of the question, and counting them would turn a week-wide filter over an
// hour of logs into a footer about ten thousand missing minutes.
func emptyBins(first, last sql.NullTime, distinct int64, width time.Duration) int64 {
	if !first.Valid || !last.Valid || width <= 0 {
		return 0
	}
	span := int64(last.Time.Sub(first.Time)/width) + 1
	if span <= distinct {
		return 0
	}
	return span - distinct
}

// statsNumeric checks that every field a numeric aggregate reads holds numbers.
//
// A field of paths averaged as a number produces a column of NULLs, which reads
// as "no data" rather than "wrong question". That is the confident wrong answer
// this project exists to avoid, so a field with no numbers in it at all is an
// error, and a field with only some is a note stating exactly how many were
// left out.
func (s *Session) statsNumeric(ctx context.Context, compiled query.StatsSQL, where string, args []any) ([]string, error) {
	if len(compiled.Numeric) == 0 {
		return nil, nil
	}

	selects := make([]string, 0, len(compiled.Numeric)*3)
	for _, n := range compiled.Numeric {
		selects = append(selects,
			"count("+n.Expr+")",
			"count(TRY_CAST("+n.Expr+" AS DOUBLE))",
			"min(CAST("+n.Expr+" AS VARCHAR))")
	}

	present := make([]int64, len(compiled.Numeric))
	numeric := make([]int64, len(compiled.Numeric))
	sample := make([]sql.NullString, len(compiled.Numeric))

	dest := make([]any, 0, len(selects))
	for i := range compiled.Numeric {
		dest = append(dest, &present[i], &numeric[i], &sample[i])
	}

	row := s.DB.QueryRow(ctx, "SELECT "+strings.Join(selects, ", ")+" FROM logs WHERE "+where, args...)
	if err := row.Scan(dest...); err != nil {
		return nil, fmt.Errorf("check that the aggregated fields hold numbers: %w", err)
	}

	var notes []string
	for i, n := range compiled.Numeric {
		switch {
		case present[i] == 0:
			notes = append(notes, fmt.Sprintf(
				"No record in this window carries %s, so %s has nothing to read.", n.Field, n.Agg))
		case numeric[i] == 0:
			return nil, &NonNumericFieldError{
				Agg: n.Agg, Field: n.Field, Values: present[i], Sample: sample[i].String,
			}
		case numeric[i] < present[i]:
			notes = append(notes, fmt.Sprintf(
				"%d of %d %s values are not numbers and are left out of %s.",
				present[i]-numeric[i], present[i], n.Field, n.Agg))
		}
	}
	return notes, nil
}

// NonNumericFieldError reports a numeric aggregate over a field that holds no
// numbers at all.
type NonNumericFieldError struct {
	// Agg is the aggregate as written, e.g. avg(path).
	Agg string
	// Field is the field it reads.
	Field string
	// Values is how many records carry the field.
	Values int64
	// Sample is one of its values, to show why it is not a number.
	Sample string
}

func (e *NonNumericFieldError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s: %s does not hold numbers — none of its %d values is one",
		e.Agg, e.Field, e.Values)
	if e.Sample != "" {
		fmt.Fprintf(&sb, ", e.g. %q", e.Sample)
	}
	fmt.Fprintf(&sb, "\ncount(%s) counts the records that carry it, and `loupe top %s` "+
		"breaks down its values", e.Field, e.Field)
	return sb.String()
}

// binAnchor decides what time buckets are aligned to, and reports a clock
// change that moves them.
//
// Buckets are anchored to local midnight in the display timezone rather than to
// the Unix epoch, so bin(1h) lands on the hour on the user's clock. Anchoring to
// the epoch instead would put every bucket boundary half an hour out in India
// and forty-five minutes out in Nepal, which is exactly the offset arithmetic
// docs/FILTER-DSL.md section 2.3 says nobody should have to do.
func (s *Session) binAnchor(ctx context.Context, plan Plan, width time.Duration) (time.Time, string, error) {
	start, end, err := s.statsSpan(ctx, plan)
	if err != nil {
		return time.Time{}, "", err
	}
	if start.IsZero() {
		return time.Time{}, "", nil
	}

	local := start.In(s.Loc)
	year, month, day := local.Date()

	// time.Date consults the tz database, so a zone whose clocks change at
	// midnight gets the instant that day actually began. No offset is computed
	// arithmetically anywhere in this path.
	origin := time.Date(year, month, day, 0, 0, 0, 0, s.Loc)

	return origin, dstBinNote(origin, end, width, s.Loc), nil
}

// statsSpan is the time range the aggregation covers: the filter's window
// where it has one, and otherwise the span of the matching records.
func (s *Session) statsSpan(ctx context.Context, plan Plan) (time.Time, time.Time, error) {
	interval := plan.Resolution.Interval
	if !interval.Start.IsZero() && !interval.End.IsZero() {
		return interval.Start, interval.End, nil
	}

	var lo, hi sql.NullTime
	row := s.DB.QueryRow(ctx,
		`SELECT min(ts), max(ts) FROM logs WHERE `+plan.SQL.Where, plan.SQL.Args...)
	if err := row.Scan(&lo, &hi); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("find the span to bucket: %w", err)
	}
	if !lo.Valid || !hi.Valid {
		return time.Time{}, time.Time{}, nil
	}

	start, end := lo.Time, hi.Time
	if !interval.Start.IsZero() {
		start = interval.Start
	}
	if !interval.End.IsZero() {
		end = interval.End
	}
	return start, end, nil
}

// dstBinNote reports a clock change inside the bucketed span.
//
// Buckets are a fixed width of real time, so after the clocks change they no
// longer line up with the local clock they were anchored to: a bin(1h) day in
// London that starts on the hour goes on ending at half past for the rest of
// the autumn. That is the honest thing for a bucket of elapsed time to do, and
// saying so is the only way the reader knows why the column looks shifted.
func dstBinNote(origin, end time.Time, width time.Duration, loc *time.Location) string {
	if origin.IsZero() || end.IsZero() || !origin.Before(end) {
		return ""
	}

	startZone, startOffset := origin.Zone()
	endZone, endOffset := end.In(loc).Zone()
	if startOffset == endOffset {
		return ""
	}

	return fmt.Sprintf(
		"The clocks change inside this window (%s to %s), so bucket boundaries "+
			"after the change sit %s off the local clock; each bucket is still "+
			"exactly %s of real time.",
		startZone, endZone,
		query.FormatDuration(time.Duration(endOffset-startOffset)*time.Second),
		width)
}
