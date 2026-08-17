// Package session is the query path shared by every front end.
//
// CLAUDE.md requires that the web UI call the same code as the CLI rather than
// its own, and ARCHITECTURE.md section 4 asks that cmd/loupe be command wiring
// only. This package is where that shared middle lives: opening a directory,
// resolving a filter against the loaded data, and running it.
//
// It returns data rather than printing it. How a window or an empty result is
// rendered is the caller's business, and the HTTP API needs the same facts as
// the terminal does.
package session

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/GrantPukka/loupe/internal/query"
	"github.com/GrantPukka/loupe/internal/schema"
	"github.com/GrantPukka/loupe/internal/source"
	"github.com/GrantPukka/loupe/internal/store"
)

// Options configures opening a set of logs.
type Options struct {
	// Paths are the directories or files to read. Several are walked and
	// interleaved onto one timeline, which is the tool's premise extended from
	// one directory to a set of them.
	Paths []string

	// Parser forces a format instead of detecting one.
	Parser string

	// SourceZones is the assumed timezone per source, for formats carrying
	// none. The empty key sets the default.
	SourceZones map[string]*time.Location

	// Location is the display timezone. Defaults to the system zone.
	Location *time.Location

	// RelativeToNow makes last: measure from the wall clock rather than the
	// newest record.
	RelativeToNow bool

	NoCache  bool
	CacheDir string

	Walk source.WalkOptions
}

// Session is an opened set of logs, ready to query.
type Session struct {
	DB   *store.DB
	Load store.Load
	Loc  *time.Location

	// Paths are the locations this session was opened over.
	Paths []string

	// Walk reports the files that were passed over and why.
	Walk source.WalkOptions

	// CacheHit and CacheReason describe whether ingestion was skipped.
	CacheHit    bool
	CacheReason string
	CachePath   string

	// Promoted are the fields given real columns by schema inference.
	Promoted []schema.Promotion

	relativeToNow bool

	// load and walkOpts are kept so a follower reads new records exactly as the
	// initial ingest did — same parser, same assumed zones. A follow that
	// resolved timestamps differently from the load above it would put live
	// records on the timeline in the wrong place.
	load     store.LoadOptions
	walkOpts source.WalkOptions

	// Resolved lazily and cached: both cost a query and several callers need
	// them.
	schema      *query.Schema
	noTimestamp int64
	haveCounts  bool
	oldest      time.Time
	newest      time.Time
}

// Open walks the path, ingests it or reuses a cached ingest, and returns a
// queryable session.
func Open(ctx context.Context, opts Options) (*Session, error) {
	if opts.Location == nil {
		opts.Location = SystemLocation()
	}

	if len(opts.Paths) == 0 {
		opts.Paths = []string{"."}
	}

	walk := opts.Walk
	var sources []source.Source

	for _, path := range opts.Paths {
		found, err := source.Walk(path, &walk)
		if err != nil {
			// One unreadable location must not stop the others. A directory
			// that has been deleted since it was subscribed is a note, not a
			// reason to refuse to open anything.
			walk.Skipped = append(walk.Skipped, source.Skip{Path: path, Reason: err.Error()})
			continue
		}
		sources = append(sources, found...)
	}

	if len(sources) == 0 {
		return nil, NoSourcesError{Paths: opts.Paths, Skipped: walk.Skipped}
	}

	loadOpts := store.LoadOptions{Parser: opts.Parser, SourceZones: opts.SourceZones}

	cached, err := store.OpenCached(ctx, sources,
		loadOpts,
		store.CacheOptions{Dir: opts.CacheDir, Disabled: opts.NoCache})
	if err != nil {
		return nil, err
	}

	// Inference runs only on a fresh ingest; a hit reads the decision back.
	promoted, err := resolvePromotions(ctx, cached.DB, cached.Hit)
	if err != nil {
		cached.DB.Close()
		return nil, err
	}

	return &Session{
		DB:            cached.DB,
		Paths:         opts.Paths,
		Load:          cached.Load,
		Loc:           opts.Location,
		Walk:          walk,
		CacheHit:      cached.Hit,
		CacheReason:   cached.Reason,
		CachePath:     cached.Path,
		Promoted:      promoted,
		relativeToNow: opts.RelativeToNow,
		load:          loadOpts,
		walkOpts:      opts.Walk,
	}, nil
}

func resolvePromotions(ctx context.Context, db *store.DB, cacheHit bool) ([]schema.Promotion, error) {
	if cacheHit {
		return db.Promotions(ctx)
	}
	promotions, _, err := db.InferAndPromote(ctx, schema.Options{})
	return promotions, err
}

func (s *Session) Close() error { return s.DB.Close() }

// Schema is the set of columns and sources a filter can reference.
func (s *Session) Schema(ctx context.Context) (query.Schema, error) {
	if s.schema != nil {
		return *s.schema, nil
	}

	fields, err := s.DB.Fields(ctx)
	if err != nil {
		return query.Schema{}, err
	}

	infos, err := s.DB.Sources(ctx)
	if err != nil {
		return query.Schema{}, err
	}

	sch := query.Schema{Fields: fields, Promoted: map[string]string{}}
	for _, p := range s.Promoted {
		sch.Promoted[p.Field] = p.Column
	}

	seen := map[string]bool{}
	for _, info := range infos {
		if !seen[info.Name] {
			seen[info.Name] = true
			sch.Sources = append(sch.Sources, info.Name)
		}
	}
	sort.Strings(sch.Sources)

	s.schema = &sch
	return sch, nil
}

// TimeContext is what time resolution needs from the loaded data.
func (s *Session) TimeContext(ctx context.Context) (query.TimeContext, error) {
	if !s.haveCounts {
		oldest, newest, noTimestamp, err := s.DB.TimeRange(ctx)
		if err != nil {
			return query.TimeContext{}, err
		}
		s.oldest, s.newest, s.noTimestamp, s.haveCounts = oldest, newest, noTimestamp, true
	}

	return query.TimeContext{
		Loc:           s.Loc,
		Oldest:        s.oldest,
		Newest:        s.newest,
		Now:           time.Now(),
		RelativeToNow: s.relativeToNow,
	}, nil
}

// NoTimestamp is how many records carry no timestamp, and are therefore
// excluded by any time filter. Callers must report it.
func (s *Session) NoTimestamp(ctx context.Context) int64 {
	if !s.haveCounts {
		// Called for the counts it caches on s. A failure here leaves
		// noTimestamp at zero, which the caller reports as "none excluded" —
		// the same thing it would report for a genuinely empty result.
		_, _ = s.TimeContext(ctx)
	}
	return s.noTimestamp
}

// Plan is a filter that has been parsed, resolved against the data, and
// compiled to parameterised SQL.
type Plan struct {
	// Filter is the expression as written.
	Filter string
	// Query is the parsed AST, before time resolution.
	Query query.Query
	// SQL is the compiled WHERE clause and its parameters.
	SQL query.SQL
	// Resolution records how the time terms were interpreted, including every
	// assumption made on the user's behalf.
	Resolution query.Resolution
}

// Plan parses and compiles a filter expression.
//
// It resolves against the loaded data, so an unknown field names the fields
// that actually exist and a bare 14:00 lands on a day the logs cover.
func (s *Session) Plan(ctx context.Context, filter string) (Plan, error) {
	parsed, err := query.Parse(filter)
	if err != nil {
		return Plan{}, err
	}

	tc, err := s.TimeContext(ctx)
	if err != nil {
		return Plan{}, err
	}

	resolved, resolution, err := query.ResolveTime(parsed, tc)
	if err != nil {
		return Plan{}, err
	}

	sch, err := s.Schema(ctx)
	if err != nil {
		return Plan{}, err
	}

	compiled, err := query.Compile(resolved, sch)
	if err != nil {
		return Plan{}, err
	}

	return Plan{Filter: filter, Query: parsed, SQL: compiled, Resolution: resolution}, nil
}

// RecordColumns is what a record listing selects.
//
// raw is absent because it duplicates message for parsed records and makes the
// table unreadable. It is one --format raw away, and always present in a
// handoff.
const RecordColumns = `ts, level, source, message`

// SortOrder is how a record listing is ordered.
type SortOrder string

const (
	// SortTime is oldest first, with untimestamped records last so they do not
	// crowd the top, then ingest order to keep them beside their neighbours.
	SortTime SortOrder = "time"
	// SortTimeDesc is newest first, which is what a live tail wants.
	SortTimeDesc SortOrder = "-time"
)

func (o SortOrder) clause() string {
	if o == SortTimeDesc {
		return ` ORDER BY ts DESC NULLS LAST, seq DESC`
	}
	return ` ORDER BY ts NULLS LAST, seq`
}

// RecordQuery is one page of records.
type RecordQuery struct {
	Limit  int
	Offset int
	Sort   SortOrder
	// Columns overrides the default selection.
	Columns string

	// Where is ANDed with the plan's own predicate, and WhereArgs are its
	// parameters. Follow mode uses it to select only the records a poll made
	// newly visible, so live output runs through the same compiled filter as
	// everything else rather than a parallel path.
	Where     string
	WhereArgs []any
}

// Records runs a plan and returns matching records.
func (s *Session) Records(ctx context.Context, plan Plan, q RecordQuery) (store.Result, error) {
	columns := q.Columns
	if columns == "" {
		columns = RecordColumns
	}

	where := plan.SQL.Where
	args := plan.SQL.Args
	if q.Where != "" {
		where = "(" + where + ") AND (" + q.Where + ")"
		args = append(append([]any{}, args...), q.WhereArgs...)
	}

	sql := `SELECT ` + columns + ` FROM logs WHERE ` + where + q.Sort.clause()

	if q.Offset > 0 {
		// A limit is required before an offset in DuckDB, and a page beyond
		// the end is an empty page rather than an error.
		limit := q.Limit
		if limit <= 0 {
			limit = -1
		}
		sql += fmt.Sprintf(" LIMIT %d OFFSET %d", limit, q.Offset)
		return s.DB.QueryResult(ctx, 0, sql, args...)
	}

	return s.DB.QueryResult(ctx, q.Limit, sql, args...)
}

// Count returns how many records a plan matches.
func (s *Session) Count(ctx context.Context, plan Plan) (int64, error) {
	var n int64
	row := s.DB.QueryRow(ctx, `SELECT count(*) FROM logs WHERE `+plan.SQL.Where, plan.SQL.Args...)
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count matching records: %w", err)
	}
	return n, nil
}

// NoSourcesError reports that a walk found nothing readable, and what it passed
// over.
//
// An empty result with no explanation is the single most misleading thing this
// tool could do, so the skipped files travel with the error.
type NoSourcesError struct {
	Paths   []string
	Skipped []source.Skip
}

func (e NoSourcesError) Error() string {
	where := strings.Join(e.Paths, ", ")
	if where == "" {
		where = "the given paths"
	}

	if len(e.Skipped) == 0 {
		return fmt.Sprintf("no log files found in %s", where)
	}
	return fmt.Sprintf("no readable log files in %s, but %d file(s) were skipped",
		where, len(e.Skipped))
}

// Follower watches this session's locations for records written after it opened.
//
// The source list is resolved on every poll rather than captured, so a service
// that starts logging mid-incident is picked up without reopening the session.
func (s *Session) Follower(ctx context.Context) (*store.Follower, error) {
	return s.DB.NewFollower(ctx, func() ([]source.Source, error) {
		walk := s.walkOpts
		var found []source.Source
		for _, path := range s.Paths {
			got, err := source.Walk(path, &walk)
			if err != nil {
				// A location that has become unreadable must not end the
				// session. The other locations are still the best information
				// available, which is the whole point during an incident.
				continue
			}
			found = append(found, got...)
		}
		return found, nil
	}, s.load)
}
