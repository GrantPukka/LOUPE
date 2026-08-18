package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/GrantPukka/loupe/internal/parse"
	"github.com/GrantPukka/loupe/internal/pattern"
	"github.com/marcboeker/go-duckdb"
)

// Ingester appends records to the logs table.
//
// It uses DuckDB's Appender rather than row-by-row INSERT, which is roughly an
// order of magnitude faster. Records are streamed through it, so memory stays
// bounded regardless of how much is being ingested.
//
// An Ingester is not safe for concurrent use. Ingest one source at a time, or
// give each goroutine its own.
type Ingester struct {
	store    *DB
	conn     driverConn
	appender *duckdb.Appender

	// meta describes the source currently being ingested.
	meta Source

	// scratch is reused between rows so that encoding the fields bag does not
	// allocate a new buffer per record.
	scratch []byte
}

// driverConn is the subset of the DuckDB connection the Appender needs. Named
// so the close path stays readable.
type driverConn interface{ Close() error }

// ZoneOrigin records where a source's assumed timezone came from, so that
// `loupe sources` can show the assumption and the user can audit it in ten
// seconds rather than discovering it an hour into an incident.
type ZoneOrigin int

const (
	// ZoneFromDefault means nobody chose: UTC was assumed.
	ZoneFromDefault ZoneOrigin = iota
	// ZoneFromFlag means --source-tz set a default for every source.
	ZoneFromFlag
	// ZoneFromFlagPerSource means --source-tz named this source specifically.
	ZoneFromFlagPerSource
)

func (z ZoneOrigin) String() string {
	switch z {
	case ZoneFromFlag:
		return "--source-tz"
	case ZoneFromFlagPerSource:
		return "--source-tz, named"
	default:
		return "default"
	}
}

// Source describes where a batch of records came from. Every field ends up in a
// column, because every one of them is filterable.
type Source struct {
	// Name is the logical source, e.g. checkout-api. Rotated files share one.
	Name string
	// File is the path the records were read from.
	File string
	// Format is the parser that read them.
	Format string
	// Zone is the timezone applied to timestamps in this source that carry
	// none. Whether it was actually needed is per-record: see
	// parse.Stats.ZoneAssumed.
	Zone string
	// ZoneSource says where Zone came from.
	ZoneSource ZoneOrigin
}

// NewIngester opens an appender against the logs table.
func (s *DB) NewIngester() (*Ingester, error) { return s.NewIngesterInto("logs") }

// NewIngesterInto opens an appender against a named table.
//
// The appender writes the base column set, so the target must have exactly that
// shape. Follow mode uses this to stage new records in a side table before
// inserting them into a logs table that schema inference has widened.
func (s *DB) NewIngesterInto(table string) (*Ingester, error) {
	conn, err := s.connector.Connect(context.Background())
	if err != nil {
		return nil, fmt.Errorf("connect for ingest: %w", err)
	}

	appender, err := duckdb.NewAppenderFromConn(conn, "", table)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("create appender on %s: %w", table, err)
	}

	return &Ingester{
		store:    s,
		conn:     conn,
		appender: appender,
		scratch:  make([]byte, 0, 4096),
	}, nil
}

// SetSource declares which source the following records come from.
func (i *Ingester) SetSource(src Source) { i.meta = src }

// Add appends one record.
func (i *Ingester) Add(e parse.Entry) error {
	fields, err := i.encodeFields(e.Fields)
	if err != nil {
		return err
	}

	// A zero timestamp becomes SQL NULL rather than year 1. ts:none selects
	// these, and a range comparison must not accidentally include them.
	var ts any
	if e.HasTimestamp() {
		ts = e.Timestamp.UTC()
	}

	shape := patternOf(e)

	err = i.appender.AppendRow(
		i.store.seq,
		ts,
		e.TimestampZoned,
		nullIfEmpty(e.Level),
		e.Message,
		i.meta.Name,
		i.meta.File,
		i.meta.Format,
		e.LineNo,
		e.Parsed,
		e.Raw,
		fields,
		shape.Text,
		shape.ID,
	)
	if err != nil {
		return fmt.Errorf("append %s line %d: %w", i.meta.File, e.LineNo, err)
	}

	i.store.seq++
	return nil
}

// patternOf templates the record's message, falling back to its raw line.
//
// A line no parser understood has no message, and templating the empty string
// would put every unparsed record into one nameless template — hiding exactly
// the records that most need looking at. The raw text is what the reader would
// see, so it is what gets templated.
func patternOf(e parse.Entry) pattern.Pattern {
	text := e.Message
	if text == "" {
		text = e.Raw
	}
	return pattern.Of(text)
}

// Flush writes buffered rows without closing the appender.
func (i *Ingester) Flush() error {
	if err := i.appender.Flush(); err != nil {
		return fmt.Errorf("flush appender: %w", err)
	}
	return nil
}

// Close flushes and releases the appender. It must be called, or buffered rows
// are lost — which would be exactly the silent data loss this project exists to
// avoid, so callers should treat a Close error as fatal rather than deferring
// it without checking.
func (i *Ingester) Close() error {
	err := i.appender.Close()
	if cerr := i.conn.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("close appender: %w", err)
	}
	return nil
}

// encodeFields renders the fields bag as JSON.
//
// An empty bag becomes NULL rather than "{}", so that field:none can be
// answered without parsing JSON, and so the column compresses well on the very
// common no-extra-fields case.
func (i *Ingester) encodeFields(fields map[string]any) (any, error) {
	if len(fields) == 0 {
		return nil, nil
	}

	i.scratch = i.scratch[:0]
	b, err := json.Marshal(fields)
	if err != nil {
		// Refusing the record would lose it. Keep it with a diagnostic in place
		// of the fields, which is visible rather than silent.
		return fmt.Sprintf(`{"_loupe_error":%q}`, err.Error()), nil
	}
	return string(b), nil
}

// nullIfEmpty maps an empty string to NULL, so that level:none distinguishes
// "no level" from the empty string.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// IngestResult reports what one source contributed.
type IngestResult struct {
	Source Source
	Stats  parse.Stats
	Took   time.Duration

	// ResumeAt is the byte offset in the source file at which a later read
	// should continue. It is the start of the last record ingested, not the end
	// of the file, so that a record still being written is re-read whole rather
	// than split — see parse.Tail.
	ResumeAt int64

	// ResumeLine is the physical line number of that record. A resumed read
	// discards rows at or after it for this file before appending, which is
	// what makes re-reading the last record idempotent instead of duplicating
	// it.
	ResumeLine int64

	// Before is this read's stats excluding its final record, which a resumed
	// read will count again. See parse.Tail.Before.
	Before parse.Stats `json:"before"`
}
