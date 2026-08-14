package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/VIGIL-OPS/loupe/internal/session"
	"github.com/VIGIL-OPS/loupe/internal/store"
)

// schemaResponse describes what a filter can reference.
type schemaResponse struct {
	// Columns are the promoted and built-in columns, with their types.
	Columns []columnInfo `json:"columns"`
	// Fields are the keys still in the JSON bag.
	Fields  []string     `json:"fields"`
	Sources []sourceInfo `json:"sources"`

	// Timezone is the display zone every timestamp in a response is rendered
	// in. The UI must show it: a user who cannot tell whose clock they are
	// reading is the failure docs/FILTER-DSL.md section 2.3 exists to prevent.
	Timezone string `json:"timezone"`

	// Range is what the data covers, so the UI can size a timeline without
	// guessing.
	Oldest time.Time `json:"oldest,omitempty"`
	Newest time.Time `json:"newest,omitempty"`

	Records     int64 `json:"records"`
	NoTimestamp int64 `json:"no_timestamp"`
	Unparsed    int64 `json:"unparsed"`
}

type columnInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Promoted distinguishes an inferred column from a built-in one.
	Promoted bool `json:"promoted"`
	// Coverage is the fraction of records carrying it, for promoted columns.
	Coverage float64 `json:"coverage,omitempty"`
}

type sourceInfo struct {
	Name   string `json:"name"`
	File   string `json:"file"`
	Format string `json:"format"`
	// Timezone reports whether the source's zone was known or assumed. The UI
	// shows this for the same reason `loupe sources` does: an assumption
	// nobody can see is one nobody can check.
	Timezone    string `json:"timezone"`
	Records     int64  `json:"records"`
	Unparsed    int64  `json:"unparsed"`
	NoTimestamp int64  `json:"no_timestamp"`
}

func (s *Server) handleSchema(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	sch, err := s.sess.Schema(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	out := schemaResponse{
		Timezone:    s.sess.Loc.String(),
		Records:     s.sess.Load.Stats.Records,
		NoTimestamp: s.sess.Load.Stats.NoTimestamp,
		Unparsed:    s.sess.Load.Stats.Unparsed,
		Fields:      sch.Fields,
	}

	for _, name := range builtinColumns {
		out.Columns = append(out.Columns, columnInfo{Name: name.name, Type: name.sqlType})
	}
	for _, p := range s.sess.Promoted {
		out.Columns = append(out.Columns, columnInfo{
			Name:     p.Field,
			Type:     p.Kind.SQLType(),
			Promoted: true,
			Coverage: p.Coverage,
		})
	}

	infos, err := s.sess.DB.Sources(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for _, info := range infos {
		out.Sources = append(out.Sources, sourceInfo{
			Name:        info.Name,
			File:        info.File,
			Format:      info.Format,
			Timezone:    info.TimezoneStatus(),
			Records:     info.Records,
			Unparsed:    info.Unparsed,
			NoTimestamp: info.NoTimestamp,
		})
	}

	if tc, err := s.sess.TimeContext(ctx); err == nil {
		out.Oldest, out.Newest = tc.Oldest, tc.Newest
	}

	writeJSON(w, http.StatusOK, out)
}

// builtinColumns is the fixed part of the schema, in the order a reader wants
// them rather than the order the table declares them.
var builtinColumns = []struct{ name, sqlType string }{
	{"ts", "TIMESTAMP"},
	{"level", "VARCHAR"},
	{"message", "VARCHAR"},
	{"source", "VARCHAR"},
	{"file", "VARCHAR"},
	{"format", "VARCHAR"},
	{"line_no", "BIGINT"},
	{"parsed", "BOOLEAN"},
	{"raw", "VARCHAR"},
}

// queryRequest is a filter or raw SQL, plus paging.
type queryRequest struct {
	// Filter is a DSL expression. Mutually exclusive with SQL.
	Filter string `json:"filter"`
	// SQL is raw DuckDB, the same escape hatch `loupe sql` provides.
	SQL string `json:"sql"`

	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
	Sort   string `json:"sort"`
	// Columns overrides the default selection, for a row-detail request.
	Columns string `json:"columns"`
}

// queryResponse is a page of records plus everything needed to trust it.
type queryResponse struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
	Total   int64    `json:"total"`
	// Truncated says the page is not the whole answer. A UI that renders rows
	// without this cannot tell the user they are looking at a slice.
	Truncated bool    `json:"truncated"`
	TookMS    float64 `json:"took_ms"`

	// Timezone names the zone timestamps should be displayed in. The rows
	// themselves carry UTC instants, so a client that ignores this renders
	// somebody else's clock without knowing it.
	Timezone string `json:"timezone"`

	// Window and Notes carry the same disclosures the CLI banner prints.
	Window *windowInfo `json:"window,omitempty"`
	Notes  []string    `json:"notes,omitempty"`
	// ExcludedNoTimestamp is how many records a time filter left out.
	ExcludedNoTimestamp int64 `json:"excluded_no_timestamp,omitempty"`

	// Explanation is set when nothing matched, so the UI can say why rather
	// than showing an empty table.
	Explanation *session.Explanation `json:"explanation,omitempty"`
}

type windowInfo struct {
	Start time.Time `json:"start,omitempty"`
	End   time.Time `json:"end,omitempty"`
	// Description is the human rendering, in both zones.
	Description string `json:"description"`
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req queryRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Filter != "" && req.SQL != "" {
		writeError(w, http.StatusBadRequest,
			fmt.Errorf("give either filter or sql, not both"))
		return
	}

	ctx := r.Context()

	if req.SQL != "" {
		res, err := s.sess.DB.QueryResult(ctx, req.Limit, req.SQL)
		if err != nil {
			// A SQL error is the user's to fix, so it comes back verbatim.
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, s.resultResponse(res))
		return
	}

	plan, err := s.sess.Plan(ctx, req.Filter)
	if err != nil {
		// A filter error carries a spelling suggestion or a working example,
		// and the UI should show all of it.
		writeError(w, http.StatusBadRequest, err)
		return
	}

	res, err := s.sess.Records(ctx, plan, session.RecordQuery{
		Limit:   req.Limit,
		Offset:  req.Offset,
		Sort:    session.SortOrder(req.Sort),
		Columns: req.Columns,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	out := s.resultResponse(res)
	out.Window = s.describeWindow(plan)
	for _, note := range plan.Resolution.Notes {
		out.Notes = append(out.Notes, note.Text)
	}
	if plan.Resolution.HasTimeFilter() {
		out.ExcludedNoTimestamp = s.sess.NoTimestamp(ctx)
	}
	if res.RowCount() == 0 && !plan.Query.IsEmpty() {
		explanation := s.sess.Explain(ctx, plan)
		out.Explanation = &explanation
	}

	writeJSON(w, http.StatusOK, out)
}

func (s *Server) describeWindow(plan session.Plan) *windowInfo {
	if !plan.Resolution.HasTimeFilter() {
		return nil
	}
	interval := plan.Resolution.Interval
	return &windowInfo{
		Start:       interval.Start,
		End:         interval.End,
		Description: interval.Describe(s.sess.Loc),
	}
}

func (s *Server) resultResponse(res store.Result) queryResponse {
	return queryResponse{
		Columns:   res.Columns,
		Rows:      res.Rows,
		Total:     res.Total,
		Truncated: res.Truncated,
		TookMS:    float64(res.Took.Microseconds()) / 1000,
		Timezone:  s.sess.Loc.String(),
	}
}

// histogramRequest asks for record counts over time.
type histogramRequest struct {
	Filter string `json:"filter"`
	// Buckets is how many intervals to divide the window into.
	Buckets int `json:"buckets"`
	// IntervalMS overrides the computed bucket width.
	IntervalMS int64 `json:"interval_ms"`
}

type histogramResponse struct {
	Buckets    []session.Bucket `json:"buckets"`
	IntervalMS int64            `json:"interval_ms"`
	Start      time.Time        `json:"start,omitempty"`
	End        time.Time        `json:"end,omitempty"`
	Max        int64            `json:"max"`
	Total      int64            `json:"total"`
	// NoTimestamp is how many matching records are not on the timeline at all.
	// A timeline that silently omits them understates the data.
	NoTimestamp int64  `json:"no_timestamp"`
	Timezone    string `json:"timezone"`
}

func (s *Server) handleHistogram(w http.ResponseWriter, r *http.Request) {
	var req histogramRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	ctx := r.Context()

	plan, err := s.sess.Plan(ctx, req.Filter)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	hist, err := s.sess.Histogram(ctx, plan, session.HistogramQuery{
		Buckets:  req.Buckets,
		Interval: time.Duration(req.IntervalMS) * time.Millisecond,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, histogramResponse{
		Buckets:     hist.Buckets,
		IntervalMS:  hist.Interval.Milliseconds(),
		Start:       hist.Start,
		End:         hist.End,
		Max:         hist.Max,
		Total:       hist.Total,
		NoTimestamp: hist.NoTimestamp,
		Timezone:    s.sess.Loc.String(),
	})
}

func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	infos, err := s.sess.DB.Sources(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	out := make([]sourceInfo, 0, len(infos))
	for _, info := range infos {
		out = append(out, sourceInfo{
			Name:        info.Name,
			File:        info.File,
			Format:      info.Format,
			Timezone:    info.TimezoneStatus(),
			Records:     info.Records,
			Unparsed:    info.Unparsed,
			NoTimestamp: info.NoTimestamp,
		})
	}

	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"records": s.sess.Load.Stats.Records,
	})
}
