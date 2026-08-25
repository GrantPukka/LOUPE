package session

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/GrantPukka/loupe/internal/query"
)

// TraceFields are the names a correlation id is conventionally stored under.
//
// Ordered by preference, which only decides ties: a dataset carrying both
// trace_id and request_id is asked which covers more records, and the order
// here settles it only when the counts are equal. Detection beats a flag,
// because a flag for this is an admission that the tool could not work out
// something it had all the information to work out.
var TraceFields = []string{
	"trace_id",
	"traceId",
	"trace-id",
	"traceid",
	"request_id",
	"requestId",
	"req_id",
	"x-request-id",
	"correlation_id",
	"correlationId",
}

// TraceField is a correlation field present in the loaded data.
type TraceField struct {
	Name string `json:"name"`
	// Records is how many records carry a value for it.
	Records int64 `json:"records"`
	// Others are the candidate fields also present, which the user may have
	// meant instead. Named so a wrong guess is visible rather than silent.
	Others []string `json:"others,omitempty"`
}

// Hop is one record in a trace.
type Hop struct {
	Seq     int64     `json:"seq"`
	Time    time.Time `json:"time,omitempty"`
	Source  string    `json:"source"`
	File    string    `json:"file"`
	Level   string    `json:"level"`
	Message string    `json:"message"`

	// Gap is the time since the previous dated hop. The first dated hop has
	// none, and neither does an undated one.
	Gap    time.Duration `json:"gap,omitempty"`
	HasGap bool          `json:"has_gap,omitempty"`

	// TextOnly marks a hop matched on the line's text rather than on the
	// correlation field, which it does not carry.
	TextOnly bool `json:"text_only,omitempty"`
}

// Dated reports whether this hop can be placed in time.
func (h Hop) Dated() bool { return !h.Time.IsZero() }

// SourceReach is what one source can say about a trace.
type SourceReach struct {
	Name string `json:"name"`
	// Hops is how many records this source contributed to the trace.
	Hops int64 `json:"hops"`
	// Carries reports whether this source records the correlation field at
	// all, for any request.
	//
	// This is the distinction the whole view turns on. A source that carries
	// correlation ids and has none for this trace probably did not handle the
	// request. A source that never carries them may well have handled it and
	// simply cannot say — an Nginx access log in the combined format has
	// nowhere to put a trace id. Reporting both as "absent" would invite the
	// reader to conclude the request skipped a service it went straight
	// through.
	Carries bool `json:"carries"`
}

// Trace is one request's path through the loaded data.
type Trace struct {
	ID    string `json:"id"`
	Field string `json:"field"`

	Hops []Hop `json:"hops"`

	// Undated is how many hops carry no timestamp. They are kept, ordered
	// last, and counted — never dropped, because a record without a clock is
	// still a record and often the interesting one.
	Undated int `json:"undated,omitempty"`

	// Span is from the first dated hop to the last.
	Span time.Duration `json:"span,omitempty"`

	// Reach classifies every source in the data against this trace.
	Reach []SourceReach `json:"reach"`

	// TextOnly is how many hops were found only by searching the records'
	// text, because they do not carry the correlation field. Their fields are
	// unavailable and the match was a substring one, both of which the reader
	// has to be told.
	TextOnly int `json:"text_only,omitempty"`
}

// Found reports whether the trace matched anything.
func (t Trace) Found() bool { return len(t.Hops) > 0 }

// Slowest is the index of the hop with the largest gap, or -1.
//
// The gap is the finding. A trace is usually five lines that all look fine and
// one four-second wait between two of them, and pointing at the wait is most of
// the value of drawing the timeline at all.
func (t Trace) Slowest() int {
	best, at := time.Duration(0), -1
	for i, h := range t.Hops {
		if h.HasGap && h.Gap > best {
			best, at = h.Gap, i
		}
	}
	return at
}

// Present are the sources that contributed a hop.
func (t Trace) Present() []SourceReach {
	return t.filterReach(func(r SourceReach) bool { return r.Hops > 0 })
}

// Silent are sources that record correlation ids but none for this trace.
// Those are the services this request most likely did not reach.
func (t Trace) Silent() []SourceReach {
	return t.filterReach(func(r SourceReach) bool { return r.Hops == 0 && r.Carries })
}

// Blind are sources that never record a correlation id. They may have handled
// this request; nothing in their output could say so either way.
func (t Trace) Blind() []SourceReach {
	return t.filterReach(func(r SourceReach) bool { return r.Hops == 0 && !r.Carries })
}

func (t Trace) filterReach(keep func(SourceReach) bool) []SourceReach {
	var out []SourceReach
	for _, r := range t.Reach {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

// DetectTraceField picks the correlation field to follow, knowing nothing about
// which id is being followed.
func (s *Session) DetectTraceField(ctx context.Context) (TraceField, error) {
	return s.DetectTraceFieldFor(ctx, "")
}

// DetectTraceFieldFor picks the correlation field to follow for a given id.
//
// Coverage decides only when the id itself cannot. A field holding the value
// the user actually pasted beats a field holding forty thousand other values,
// however well covered the latter is: asked to follow
// req-7f3c9a2e-…, picking trace_id because it covers more records and then
// reporting that no record carries that trace_id is a confidently wrong answer
// to a question the tool had all the information to answer. An id present in
// two fields still falls back to coverage, which is the only tie-break left.
//
// Candidates are tested through the ordinary filter path rather than by
// inspecting the schema directly, so a promoted column and a key still in the
// JSON bag are found the same way and neither needs special handling here.
func (s *Session) DetectTraceFieldFor(ctx context.Context, id string) (TraceField, error) {
	type candidate struct {
		name    string
		count   int64
		matches int64
	}
	var found []candidate

	for _, name := range TraceFields {
		plan, err := s.Plan(ctx, renderTerm(name, "*"))
		if err != nil {
			// Not a field this data has. That is the ordinary answer for most
			// of the candidate list.
			continue
		}

		n, err := s.Count(ctx, plan)
		if err != nil {
			return TraceField{}, err
		}
		if n == 0 {
			continue
		}

		c := candidate{name: name, count: n}

		if id != "" {
			holding, err := s.Plan(ctx, renderTerm(name, id))
			if err == nil {
				if c.matches, err = s.Count(ctx, holding); err != nil {
					return TraceField{}, err
				}
			}
		}

		found = append(found, c)
	}

	if len(found) == 0 {
		return TraceField{}, &NoTraceFieldError{Tried: TraceFields}
	}

	// Holding the id wins; then coverage; then the candidate order, which is
	// what SliceStable preserves.
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].matches != found[j].matches {
			return found[i].matches > found[j].matches
		}
		return found[i].count > found[j].count
	})

	out := TraceField{Name: found[0].name, Records: found[0].count}
	for _, c := range found[1:] {
		out.Others = append(out.Others, c.name)
	}
	return out, nil
}

// NoTraceFieldError reports that nothing in the data looks like a correlation
// id, and says what was looked for.
type NoTraceFieldError struct {
	Tried []string
}

func (e *NoTraceFieldError) Error() string {
	return fmt.Sprintf("no correlation field in this data; looked for %s\n"+
		"name one with --field if it is called something else",
		joinList(e.Tried))
}

// Trace follows one correlation id through every source.
func (s *Session) Trace(ctx context.Context, id, field string) (Trace, error) {
	if field == "" {
		detected, err := s.DetectTraceFieldFor(ctx, id)
		if err != nil {
			return Trace{}, err
		}
		field = detected.Name
	}

	out := Trace{ID: id, Field: field}

	plan, err := s.Plan(ctx, renderTerm(field, id))
	if err != nil {
		return Trace{}, err
	}

	// Oldest first, with undated records last: SortTime is exactly that order,
	// so a trace and a record listing agree about what "in order" means.
	res, err := s.Records(ctx, plan, RecordQuery{
		Sort:    SortTime,
		Columns: "seq, ts, source, file, level, message",
	})
	if err != nil {
		return Trace{}, err
	}

	out.Hops = hopsFrom(res.Columns, res.Rows)

	// An id can be in the file without being in a field. An unparsed line, or a
	// format with nowhere to put a correlation id, still mentions it in its
	// text, and a trace that lists only the records that carry the field shows
	// one hop of six and reads as though the other five never happened.
	//
	// An empty or one-character id would match most of the corpus as a
	// substring, which is not a trace, so this declines to guess.
	if len(id) > 1 {
		raw, err := s.traceFromText(ctx, id)
		if err != nil {
			return Trace{}, err
		}
		out.Hops, out.TextOnly = mergeHops(out.Hops, raw)
	}

	for i := range out.Hops {
		if !out.Hops[i].Dated() {
			out.Undated++
		}
	}
	fillGaps(out.Hops)
	out.Span = spanOf(out.Hops)

	reach, err := s.traceReach(ctx, field, plan, out.Hops)
	if err != nil {
		return Trace{}, err
	}
	out.Reach = reach

	return out, nil
}

// mergeHops folds text matches into the field matches, in time order.
//
// It returns how many hops only the text search found, because that is the
// caveat the reader needs: those records have no fields to inspect, and the
// match was a substring rather than a field equality.
func mergeHops(fielded, text []Hop) ([]Hop, int) {
	seen := make(map[int64]bool, len(fielded))
	for _, h := range fielded {
		seen[h.Seq] = true
	}

	merged := fielded
	extra := 0
	for _, h := range text {
		if seen[h.Seq] {
			continue
		}
		h.TextOnly = true
		merged = append(merged, h)
		extra++
	}

	if extra == 0 {
		return merged, 0
	}

	// Undated hops sort last and among themselves by ingest order, which is
	// what SortTime means everywhere else in this tool.
	sort.SliceStable(merged, func(i, j int) bool {
		a, b := merged[i], merged[j]
		switch {
		case a.Dated() != b.Dated():
			return a.Dated()
		case a.Dated() && !a.Time.Equal(b.Time):
			return a.Time.Before(b.Time)
		default:
			return a.Seq < b.Seq
		}
	})
	return merged, extra
}

// traceFromText looks for the id in the text of the records themselves.
//
// The term is quoted, so it is matched as a literal rather than as a regex or
// as DSL vocabulary: a correlation id is an opaque token and must not be
// reinterpreted on the way through.
func (s *Session) traceFromText(ctx context.Context, id string) ([]Hop, error) {
	term := &query.FreeTerm{Value: query.Value{Text: id, Quoted: true}}

	plan, err := s.Plan(ctx, term.String())
	if err != nil {
		return nil, err
	}

	res, err := s.Records(ctx, plan, RecordQuery{
		Sort:    SortTime,
		Columns: "seq, ts, source, file, level, message",
	})
	if err != nil {
		return nil, err
	}
	return hopsFrom(res.Columns, res.Rows), nil
}

// traceReach counts, per source, how much of this trace it saw and whether it
// could have seen any of it.
// hops are counted from the merged list rather than re-queried, so a source
// that only mentioned the id in its text still shows as present.
func (s *Session) traceReach(ctx context.Context, field string, plan Plan, hops []Hop) ([]SourceReach, error) {
	present, err := s.Plan(ctx, renderTerm(field, "*"))
	if err != nil {
		return nil, err
	}

	perSource := map[string]int64{}
	for _, h := range hops {
		perSource[h.Source]++
	}

	// Both predicates are compiled from the filter language, so their values
	// are parameters. The argument order follows the order the predicates
	// appear in the statement, which is what DuckDB binds by.
	args := append(append([]any{}, plan.SQL.Args...), present.SQL.Args...)

	rows, err := s.DB.Query(ctx, `
		SELECT source,
		       count(*) FILTER (WHERE `+plan.SQL.Where+`) AS hops,
		       count(*) FILTER (WHERE `+present.SQL.Where+`) AS carries
		FROM logs
		GROUP BY source
		ORDER BY source`, args...)
	if err != nil {
		return nil, fmt.Errorf("measure trace reach: %w", err)
	}
	defer rows.Close()

	var out []SourceReach
	for rows.Next() {
		var (
			r       SourceReach
			carries int64
		)
		if err := rows.Scan(&r.Name, &r.Hops, &carries); err != nil {
			return nil, fmt.Errorf("scan trace reach: %w", err)
		}
		r.Carries = carries > 0
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trace reach: %w", err)
	}
	return out, nil
}

// hopsFrom builds hops from a record listing, reading columns by name so a
// change to the selection cannot silently shift a value into the wrong field.
func hopsFrom(columns []string, rows [][]any) []Hop {
	at := map[string]int{}
	for i, c := range columns {
		at[c] = i
	}

	get := func(row []any, name string) any {
		i, ok := at[name]
		if !ok || i >= len(row) {
			return nil
		}
		return row[i]
	}

	out := make([]Hop, 0, len(rows))
	for _, row := range rows {
		h := Hop{
			Source:  text(get(row, "source")),
			File:    text(get(row, "file")),
			Level:   text(get(row, "level")),
			Message: text(get(row, "message")),
		}
		if seq, ok := get(row, "seq").(int64); ok {
			h.Seq = seq
		}
		if ts, ok := get(row, "ts").(time.Time); ok {
			h.Time = ts
		}
		out = append(out, h)
	}
	return out
}

// fillGaps sets each dated hop's distance from the dated hop before it.
func fillGaps(hops []Hop) {
	var previous time.Time

	for i := range hops {
		if !hops[i].Dated() {
			continue
		}
		if !previous.IsZero() {
			hops[i].Gap = hops[i].Time.Sub(previous)
			hops[i].HasGap = true
		}
		previous = hops[i].Time
	}
}

// spanOf is first dated hop to last.
func spanOf(hops []Hop) time.Duration {
	var first, last time.Time
	for _, h := range hops {
		if !h.Dated() {
			continue
		}
		if first.IsZero() {
			first = h.Time
		}
		last = h.Time
	}
	if first.IsZero() || last.IsZero() {
		return 0
	}
	return last.Sub(first)
}

// renderTerm builds a filter term for a field and value.
//
// Rendered through the AST rather than pasted together, so a field name or an
// id containing a quote, a space, or a colon still produces a term that parses
// back to what was meant. A trace id is usually plain hex; "usually" is not a
// reason to build a query string by hand.
func renderTerm(field, value string) string {
	term := &query.FieldTerm{
		Key:    field,
		Values: []query.Value{{Text: value, Quoted: value != "*" && value != "none"}},
	}
	return term.String()
}

func joinList(names []string) string {
	if len(names) <= 1 {
		return fmt.Sprint(names)
	}
	out := ""
	for i, n := range names {
		switch {
		case i == 0:
			out = n
		case i == len(names)-1:
			out += " and " + n
		default:
			out += ", " + n
		}
	}
	return out
}

// TraceHandoff builds a pasteable extract for one request.
//
// It is the ordinary handoff for the records the trace matched, with the
// timeline and its disclosures attached. Sharing the record path rather than
// building a second one means the extract cannot show records the same filter
// would not, which is the property docs/HANDOFF.md asks for.
func (s *Session) TraceHandoff(ctx context.Context, t Trace, opts HandoffOptions) (Handoff, error) {
	plan, err := s.Plan(ctx, renderTerm(t.Field, t.ID))
	if err != nil {
		return Handoff{}, err
	}

	out, err := s.Handoff(ctx, plan, opts)
	if err != nil {
		return Handoff{}, err
	}

	out.Trace = &HandoffTrace{
		ID:      t.ID,
		Field:   t.Field,
		Undated: t.Undated,
		Blind:   reachNames(t.Blind()),
		Silent:  reachNames(t.Silent()),
	}
	if t.Span > 0 {
		out.Trace.Span = HumanDuration(t.Span)
	}

	slowest := t.Slowest()
	for i, h := range t.Hops {
		hop := HandoffHop{
			Source:  h.Source,
			Level:   h.Level,
			Message: h.Message,
			Slowest: i == slowest,
		}
		if h.Dated() {
			hop.Local = h.Time.In(s.Loc).Format("2006-01-02 15:04:05.000 MST")
			hop.UTC = h.Time.UTC().Format("15:04:05.000")
		}
		if h.HasGap {
			hop.Gap = HumanDuration(h.Gap)
		}
		out.Trace.Hops = append(out.Trace.Hops, hop)
	}

	return out, nil
}

func reachNames(reach []SourceReach) []string {
	if len(reach) == 0 {
		return nil
	}
	out := make([]string, len(reach))
	for i, r := range reach {
		out[i] = r.Name
	}
	return out
}

// HumanDuration renders a gap at a precision a reader can compare at a glance.
//
// Full precision on a four-millisecond wait and on a four-second one makes the
// two hard to tell apart, which defeats the point of putting them in a column
// together.
func HumanDuration(d time.Duration) string {
	switch {
	case d >= time.Minute:
		return d.Round(time.Second).String()
	case d >= time.Second:
		return d.Round(10 * time.Millisecond).String()
	case d >= time.Millisecond:
		return d.Round(time.Millisecond).String()
	default:
		return d.Round(time.Microsecond).String()
	}
}
