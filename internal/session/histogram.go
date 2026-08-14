package session

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/VIGIL-OPS/loupe/internal/parse"
	"github.com/VIGIL-OPS/loupe/internal/query"
)

// DefaultBuckets is how many columns a histogram gets when the caller does not
// say. Sixty is a comfortable terminal width and a sensible sparkline density.
const DefaultBuckets = 60

// Bucket is one interval of the timeline.
type Bucket struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Count int64     `json:"count"`
	// Levels breaks the count down by severity, which is what colours the
	// timeline and makes an error cluster visible at a glance.
	Levels map[string]int64 `json:"levels,omitempty"`
}

// Histogram is a record count over time.
type Histogram struct {
	Buckets  []Bucket      `json:"buckets"`
	Interval time.Duration `json:"interval"`
	Start    time.Time     `json:"start"`
	End      time.Time     `json:"end"`
	// Max is the largest bucket count, for scaling a bar chart.
	Max int64 `json:"max"`
	// Total is how many records the histogram covers.
	Total int64 `json:"total"`
	// NoTimestamp is how many matching records could not be placed on the
	// timeline at all. A timeline that silently omits them would understate
	// the data.
	NoTimestamp int64 `json:"no_timestamp"`
}

// HistogramQuery configures bucketing.
type HistogramQuery struct {
	// Buckets is the number of intervals. Defaults to DefaultBuckets.
	Buckets int
	// Interval overrides the computed bucket width.
	Interval time.Duration
}

// Histogram counts matching records over time.
//
// The window is the resolved filter's window where it has one, and otherwise
// the span of the matching records themselves — so a histogram with no time
// filter still shows the shape of the data rather than an arbitrary range.
func (s *Session) Histogram(ctx context.Context, plan Plan, q HistogramQuery) (Histogram, error) {
	start, end, err := s.histogramWindow(ctx, plan)
	if err != nil {
		return Histogram{}, err
	}

	out := Histogram{Start: start, End: end}
	if start.IsZero() || !start.Before(end) {
		return out, nil
	}

	interval := q.Interval
	if interval <= 0 {
		buckets := q.Buckets
		if buckets <= 0 {
			buckets = DefaultBuckets
		}
		interval = bucketWidth(end.Sub(start), buckets)
	}
	out.Interval = interval

	counts, err := s.bucketCounts(ctx, plan, start, interval)
	if err != nil {
		return Histogram{}, err
	}

	// Every interval gets a bucket, including the empty ones. A timeline that
	// omits quiet periods compresses time and makes a burst look continuous.
	for at := start; at.Before(end); at = at.Add(interval) {
		b := Bucket{Start: at, End: at.Add(interval)}
		if found, ok := counts[at.UnixNano()]; ok {
			b.Count, b.Levels = found.total, found.levels
		}
		if b.Count > out.Max {
			out.Max = b.Count
		}
		out.Total += b.Count
		out.Buckets = append(out.Buckets, b)
	}

	if err := s.DB.QueryRow(ctx,
		`SELECT count(*) FROM logs WHERE (`+plan.SQL.Where+`) AND ts IS NULL`,
		plan.SQL.Args...).Scan(&out.NoTimestamp); err != nil {
		return Histogram{}, fmt.Errorf("count records with no timestamp: %w", err)
	}

	return out, nil
}

// histogramWindow decides the span to bucket over.
func (s *Session) histogramWindow(ctx context.Context, plan Plan) (time.Time, time.Time, error) {
	interval := plan.Resolution.Interval

	if !interval.Start.IsZero() && !interval.End.IsZero() {
		return interval.Start, interval.End, nil
	}

	// Without a bounded filter, fall back to the span of what actually
	// matched, which keeps an unfiltered histogram meaningful.
	var lo, hi sql.NullTime
	row := s.DB.QueryRow(ctx,
		`SELECT min(ts), max(ts) FROM logs WHERE `+plan.SQL.Where, plan.SQL.Args...)
	if err := row.Scan(&lo, &hi); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("histogram window: %w", err)
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

	// The newest record must land inside the last bucket rather than just past
	// the end of the range.
	return start, end.Add(time.Nanosecond), nil
}

type bucketCount struct {
	total  int64
	levels map[string]int64
}

// bucketCounts groups matching records by interval and level.
func (s *Session) bucketCounts(ctx context.Context, plan Plan, start time.Time, interval time.Duration) (map[int64]*bucketCount, error) {
	// The interval and origin are numbers computed here, never user text, so
	// they are formatted into the statement; DuckDB does not accept a
	// placeholder in an INTERVAL literal. Everything from the filter stays
	// parameterised.
	sqlText := fmt.Sprintf(`
		SELECT time_bucket(INTERVAL '%d' MICROSECOND, ts, TIMESTAMP '%s') AS bucket,
		       level,
		       count(*)
		FROM logs
		WHERE (%s) AND ts IS NOT NULL
		GROUP BY 1, 2`,
		interval.Microseconds(),
		start.UTC().Format("2006-01-02 15:04:05.999999"),
		plan.SQL.Where)

	rows, err := s.DB.Query(ctx, sqlText, plan.SQL.Args...)
	if err != nil {
		return nil, fmt.Errorf("bucket records: %w", err)
	}
	defer rows.Close()

	out := map[int64]*bucketCount{}
	for rows.Next() {
		var (
			bucket time.Time
			level  sql.NullString
			count  int64
		)
		if err := rows.Scan(&bucket, &level, &count); err != nil {
			return nil, fmt.Errorf("scan bucket: %w", err)
		}

		key := bucket.UnixNano()
		bc := out[key]
		if bc == nil {
			bc = &bucketCount{levels: map[string]int64{}}
			out[key] = bc
		}
		bc.total += count

		// A record with no level is counted in the total but named separately,
		// so the breakdown always sums to the total.
		name := "none"
		if level.Valid && level.String != "" {
			name = level.String
		}
		bc.levels[name] += count
	}

	return out, rows.Err()
}

// bucketWidth picks a round interval that divides the window into roughly the
// requested number of buckets.
//
// Rounding to a recognisable unit matters more than hitting the count exactly:
// a timeline whose columns are 7.3 seconds wide is one nobody can reason about,
// and the bucket boundaries are what a user drags to.
func bucketWidth(window time.Duration, buckets int) time.Duration {
	if buckets < 1 {
		buckets = 1
	}
	ideal := window / time.Duration(buckets)

	steps := []time.Duration{
		time.Millisecond, 10 * time.Millisecond, 100 * time.Millisecond,
		time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second,
		15 * time.Second, 30 * time.Second,
		time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute,
		15 * time.Minute, 30 * time.Minute,
		time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour,
		24 * time.Hour, 7 * 24 * time.Hour,
	}

	for _, step := range steps {
		if step >= ideal {
			return step
		}
	}
	return steps[len(steps)-1]
}

// LevelOrder returns the levels present in a histogram, most severe last, so a
// stacked bar builds up in a consistent order.
func LevelOrder(h Histogram) []string {
	seen := map[string]bool{}
	for _, b := range h.Buckets {
		for level := range b.Levels {
			seen[level] = true
		}
	}

	var out []string
	for _, level := range append([]string{"none"}, parse.Levels...) {
		if seen[level] {
			out = append(out, level)
			delete(seen, level)
		}
	}
	// Anything a source invented, in no particular order but after the known
	// ones so the familiar colours stay put.
	for level := range seen {
		out = append(out, level)
	}
	return out
}

// Window is the resolved time window a plan covers, for reporting.
func (p Plan) Window() query.Interval { return p.Resolution.Interval }
