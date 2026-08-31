package parse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"time"
)

// MaxLineBytes caps one logical line. A single line longer than this is
// truncated rather than dropped, and the record says so.
//
// Log lines this long are pathological — usually a serialised stack trace or an
// embedded payload — but they do occur, and refusing to read the rest of the
// file because of one is not acceptable.
const MaxLineBytes = 8 << 20 // 8MiB

// Entry is a Record plus everything about where it came from and how well it
// was understood. The store ingests these.
type Entry struct {
	Record

	// LineNo is the 1-based physical line where the record starts.
	LineNo int64

	// Raw is the original text, always kept, exactly as it appeared. Handoffs
	// include it because the receiver may not trust our parser, and they are
	// right not to.
	Raw string

	// Parsed is false when no parser could make sense of the line. The record
	// is still ingested and still queryable; only its fields are missing.
	Parsed bool

	// Truncated is true when the line exceeded MaxLineBytes.
	Truncated bool
}

// Stats counts what happened while reading one source. Every number here ends
// up in front of the user: silence about skipped or unparsed records is how a
// log tool produces confident wrong conclusions.
type Stats struct {
	Lines        int64 `json:"lines"`        // physical lines read
	Records      int64 `json:"records"`      // logical records produced
	Unparsed     int64 `json:"unparsed"`     // records no parser understood
	NoTimestamp  int64 `json:"no_timestamp"` // records with no extractable timestamp
	Continuation int64 `json:"continuation"` // physical lines folded into a preceding record
	Truncated    int64 `json:"truncated"`    // lines longer than MaxLineBytes
	Blank        int64 `json:"blank"`        // blank lines, skipped

	// ZoneAssumed counts records whose timestamp carried no timezone and was
	// therefore resolved using the source's assumed zone. Non-zero means the
	// displayed times depend on an assumption, and FILTER-DSL section 2.5
	// requires saying so.
	ZoneAssumed int64 `json:"zone_assumed"`

	// ZoneAbbrevs counts, per abbreviation, the records that carried a zone
	// abbreviation nothing could resolve. A source reported as "read as UTC
	// (default)" whose records all say AEST is one flag away from being right,
	// and this is what lets the status line say so.
	ZoneAbbrevs map[string]int64 `json:"zone_abbrevs,omitempty"`

	// InvalidUTF8 counts records whose text was not valid UTF-8 and was stored
	// with U+FFFD in place of the offending bytes. The original bytes are kept
	// hex-encoded in the fields bag, so nothing is lost — but a replacement
	// character the reader did not put there is a fact about the data, and an
	// undisclosed one would make a search for the original text fail for a
	// reason nobody could see.
	//
	// It is counted at the store boundary rather than here, because that is
	// where the constraint lives: a Go string holds arbitrary bytes quite
	// happily, and DuckDB does not.
	InvalidUTF8 int64 `json:"invalid_utf8"`
}

// ReaderOptions configures a read.
type ReaderOptions struct {
	// Parser is the format to use. Required.
	Parser Parser

	// Loc is the timezone assumed for formats that carry none. Defaults to UTC,
	// because servers overwhelmingly run UTC and the wrong default here is
	// worse than a slightly surprising one.
	Loc *time.Location

	// StartLine offsets the reported line numbers, for concatenating rotated
	// files into one logical source.
	StartLine int64

	// Workers is how many goroutines parse head lines at once. Zero or one
	// reads serially.
	//
	// Only the parsing is shared out. Deciding where records begin stays on one
	// goroutine, because that decision is sequential by nature — a continuation
	// line belongs to whatever came before it — and so does emitting them, so
	// that seq, the Tail and every counter come out in the order a serial read
	// would produce. parallel_test.go holds the two paths to being identical.
	//
	// Parsing in a pool costs latency: a record is not emitted until its batch
	// is complete. That is fine for a file and wrong for a stream, where the
	// point is to show a line the moment it arrives, so callers reading a
	// stream leave this at zero.
	Workers int
}

// parseBatch is how many records are parsed per round when Workers > 1.
//
// Large enough that the hand-off costs nothing next to the parsing, small
// enough that the entries waiting in it are a rounding error against a file.
const parseBatch = 256

// Tail is where to resume reading a source that may since have grown.
//
// It points at the *start* of the last record emitted, not past its end, and so
// re-reading from it reproduces that record. That is deliberate. A record can
// span many physical lines — a Java stack trace is one record — and a file read
// while it is being written can end mid-record. Resuming past such a record
// would leave its remaining lines with nothing to attach to, and they would be
// ingested as separate junk records.
//
// Re-reading one record is cheap. The caller makes it idempotent by discarding
// rows at or after Line for that file before appending, which is why Line
// travels with Offset rather than being derivable from it.
type Tail struct {
	// Offset is the byte position, relative to the start of r, where the last
	// emitted record began.
	Offset int64

	// Line is that record's physical line number, counting from StartLine.
	Line int64

	// Before is the stats as they stood before the last record was read.
	//
	// A resumed read re-reads that record and counts it again. Adding Before to
	// the resumed read's stats therefore totals correctly, where adding the full
	// stats would count the boundary record twice. These numbers are the ones
	// the status line reports, so an off-by-one here is a claim about the data
	// that is not true.
	Before Stats
}

// ReadAll streams r through the parser, calling fn for each record.
//
// The governing rule: a malformed line never aborts a file. Anything the parser
// rejects becomes an Entry with Parsed false and its raw text intact, and
// reading continues.
//
// It streams. Memory stays bounded regardless of input size.
//
// The returned Tail is where a later read should resume; see its documentation
// for why it points at the last record rather than past it.
func ReadAll(r io.Reader, opts ReaderOptions, fn func(Entry) error) (stats Stats, tail Tail, err error) {
	if opts.Parser == nil {
		return stats, tail, fmt.Errorf("read: no parser given")
	}
	loc := opts.Loc
	if loc == nil {
		loc = time.UTC
	}
	continuer, _ := opts.Parser.(Continuer)

	br := bufio.NewReaderSize(r, 256*1024)
	lineNo := opts.StartLine

	// Records wait here until enough have arrived to parse them together. One
	// worker means a batch of one, which is the serial read: complete a record,
	// parse it, emit it, and only then read on.
	batchSize, workers := 1, opts.Workers
	if workers > 1 {
		batchSize = parseBatch
	}
	queue := make([]pending, 0, batchSize)

	// open is the record being accumulated, so that continuation lines can be
	// appended to it before it joins the queue. -1 when there is none.
	open := -1

	// consumed counts every byte taken from r.
	var consumed int64

	// Counting is split between the two goroutines so neither has to lock.
	// This one owns what reading knows — lines, blanks, continuations,
	// truncations. The emitter owns what a parsed record knows, and the two
	// are added together once it has finished. pending.scan carries the
	// reader's half across so the Tail can be built from both.
	em := newEmitter(workers > 1, fn)

	// complete moves the open record to the queue, and hands the queue on once
	// it is full: parsed here, then counted and emitted in order.
	complete := func(force bool) error {
		if open >= 0 {
			open = -1
		}
		if len(queue) == 0 || (!force && len(queue) < batchSize) {
			return nil
		}
		batch := parseBatchOf(queue, opts.Parser, loc, workers)
		if err := em.send(batch); err != nil {
			return err
		}
		// The emitter holds the batch until it has written it, so the next one
		// gets its own backing array rather than overwriting a batch in flight.
		queue = make([]pending, 0, batchSize)
		return nil
	}

	for {
		lineStart := consumed
		line, truncated, n, err := readLine(br)
		consumed += n
		if len(line) == 0 && err == io.EOF {
			break
		}

		lineNo++
		stats.Lines++
		if truncated {
			stats.Truncated++
		}

		trimmed := bytes.TrimRight(line, "\r")

		if len(bytes.TrimSpace(trimmed)) == 0 {
			// A blank line is not a record. Counted so the totals reconcile
			// against the file's line count.
			stats.Blank++
			if err == io.EOF {
				break
			}
			continue
		}

		// A continuation line belongs to the record above it, not to itself.
		// It is held as text and appended after the head line is parsed, which
		// is the order the serial read used and the order the message needs:
		// the parser sets the message, the trace goes underneath it.
		if open >= 0 && continuer != nil && continuer.IsContinuation(trimmed) {
			stats.Continuation++
			queue[open].cont += "\n" + string(trimmed)
			if err == io.EOF {
				break
			}
			continue
		}

		// This line starts a new record, so the previous one is complete.
		if cerr := complete(false); cerr != nil {
			recStats, tail, _, _ := em.close()
			stats.Records, stats.Unparsed = recStats.Records, recStats.Unparsed
			stats.NoTimestamp, stats.ZoneAssumed = recStats.NoTimestamp, recStats.ZoneAssumed
			stats.ZoneAbbrevs = recStats.ZoneAbbrevs
			return stats, tail, cerr
		}

		// The totals as they stood before this record: the scan counters as of
		// this line, backing out the line's own accounting because a resumed
		// read starts here and counts it again. The record counters are filled
		// in at drain, where they are known and in order.
		scan := stats
		scan.Lines--
		if truncated {
			scan.Truncated--
		}

		queue = append(queue, pending{
			head:      append([]byte(nil), trimmed...),
			lineNo:    lineNo,
			truncated: truncated,
			start:     lineStart,
			scan:      scan,
		})
		open = len(queue) - 1

		// A format with no continuation lines has a complete record the moment
		// its line is read, so holding it back until the next one arrives buys
		// nothing — and on a stream it costs everything. `kubectl logs -f` on a
		// service that logs once a minute would always be showing the record
		// before last, which reads as the tail being stuck.
		//
		// Log4j and anything else that can continue still waits, because a
		// stack trace emitted before its own trace would be worse than late.
		if continuer == nil {
			if cerr := complete(false); cerr != nil {
				recStats, tail, _, _ := em.close()
				stats.Records, stats.Unparsed = recStats.Records, recStats.Unparsed
				stats.NoTimestamp, stats.ZoneAssumed = recStats.NoTimestamp, recStats.ZoneAssumed
				stats.ZoneAbbrevs = recStats.ZoneAbbrevs
				return stats, tail, cerr
			}
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			readErr := fmt.Errorf("read line %d: %w", lineNo, err)
			if cerr := complete(true); cerr != nil {
				readErr = cerr
			}
			recStats, tail, _, _ := em.close()
			stats.Records, stats.Unparsed = recStats.Records, recStats.Unparsed
			stats.NoTimestamp, stats.ZoneAssumed = recStats.NoTimestamp, recStats.ZoneAssumed
			stats.ZoneAbbrevs = recStats.ZoneAbbrevs
			return stats, tail, readErr
		}
	}

	cerr := complete(true)

	// Whatever the emitter counted, plus whatever reading counted. Joined
	// before either is read, so there is nothing to synchronise afterwards.
	recStats, tail, emitted, emitErr := em.close()
	stats.Records = recStats.Records
	stats.Unparsed = recStats.Unparsed
	stats.NoTimestamp = recStats.NoTimestamp
	stats.ZoneAssumed = recStats.ZoneAssumed
	stats.ZoneAbbrevs = recStats.ZoneAbbrevs

	switch {
	case cerr != nil:
		return stats, tail, cerr
	case emitErr != nil:
		return stats, tail, emitErr
	}

	// Nothing was emitted, so there is no record to re-read. Resume past what
	// was consumed — blank lines and nothing else — rather than at zero, which
	// would re-count them on every pass.
	if !emitted {
		tail = Tail{Offset: consumed, Line: lineNo + 1, Before: stats}
	}
	return stats, tail, nil
}

// pending is a record whose extent is known and whose head line has not been
// parsed yet.
type pending struct {
	head      []byte
	cont      string
	lineNo    int64
	truncated bool
	start     int64
	// scan holds the counters that are known while reading — lines, blanks,
	// continuations, truncations — as they stood before this record. Its
	// record counters are stale by construction and are replaced at drain.
	scan Stats
}

// batch is a run of records whose head lines have been parsed.
type batch struct {
	queue   []pending
	records []Record
	parsed  []bool
}

// parseBatchOf parses every head line in the queue.
//
// This is the half of reading that goes wide: a head line is parsed on its own,
// by a stateless parser, with no reference to any other line. Deciding where
// records begin and emitting them stay sequential, because those are facts
// about the file's order rather than about any one record. See
// ReaderOptions.Workers.
func parseBatchOf(queue []pending, p Parser, loc *time.Location, workers int) batch {
	b := batch{
		queue:   queue,
		records: make([]Record, len(queue)),
		parsed:  make([]bool, len(queue)),
	}

	parseOne := func(i int) {
		rec, perr := p.Parse(queue[i].head)
		switch {
		case perr == nil:
			b.records[i], b.parsed[i] = applyAssumedZone(rec, loc), true

		case errors.Is(perr, ErrPartial):
			// The line stopped early and the parser salvaged what preceded the
			// cut. The fields are kept so the record is findable and placeable;
			// Parsed stays false so the damage is still counted and still
			// reachable through parsed:false.
			b.records[i] = applyAssumedZone(rec, loc)

		default:
			// Not an error worth propagating: keep the raw text and move on.
			b.records[i] = Record{Message: string(queue[i].head), Fields: map[string]any{}}
		}
		if b.records[i].Fields == nil {
			b.records[i].Fields = map[string]any{}
		}
	}

	if workers > 1 && len(queue) > 1 {
		parseRange(len(queue), workers, parseOne)
	} else {
		for i := range queue {
			parseOne(i)
		}
	}
	return b
}

// emitter counts parsed records and hands them to the caller, in order.
//
// When parsing is spread across workers this runs on a goroutine of its own, so
// that appending one batch to the database overlaps parsing the next — the
// append was a fifth of the ingest and every bit of it was waiting. When it is
// not, it runs inline, because a stream's whole point is that a record appears
// the moment it arrives and a hand-off would hold it back.
type emitter struct {
	fn      func(Entry) error
	async   bool
	ch      chan batch
	done    chan struct{}
	stats   Stats
	tail    Tail
	emitted bool
	err     error
}

func newEmitter(async bool, fn func(Entry) error) *emitter {
	e := &emitter{fn: fn, async: async}
	if !async {
		return e
	}

	// One in flight: enough to overlap the append with the next parse, and no
	// more, so a slow consumer cannot make the reader buffer the file.
	e.ch = make(chan batch, 1)
	e.done = make(chan struct{})
	go func() {
		defer close(e.done)
		for b := range e.ch {
			if e.err != nil {
				// Keep draining so the reader is never blocked writing into a
				// channel nobody is reading.
				continue
			}
			e.err = e.write(b)
		}
	}()
	return e
}

// send hands a parsed batch on, and reports an error the emitter has already
// hit so the read stops rather than filling a database it cannot write to.
func (e *emitter) send(b batch) error {
	if !e.async {
		if e.err == nil {
			e.err = e.write(b)
		}
		return e.err
	}
	e.ch <- b
	return nil
}

// close finishes the emitter and returns everything it owns.
func (e *emitter) close() (Stats, Tail, bool, error) {
	if e.async {
		close(e.ch)
		<-e.done
	}
	return e.stats, e.tail, e.emitted, e.err
}

// write counts and emits one batch. Only the emitter calls it, so the fields it
// touches need no synchronising.
func (e *emitter) write(b batch) error {
	for i, q := range b.queue {
		entry := Entry{
			LineNo:    q.lineNo,
			Raw:       string(q.head) + q.cont,
			Truncated: q.truncated,
			Record:    b.records[i],
			Parsed:    b.parsed[i],
		}
		entry.Message += q.cont

		// The scan counters as of this record's head line, carried over from
		// the reader, plus the record counters as they stand now — which is to
		// say, after every record before this one and before this one itself.
		before := e.stats
		before.Lines, before.Blank = q.scan.Lines, q.scan.Blank
		before.Continuation, before.Truncated = q.scan.Continuation, q.scan.Truncated

		e.tail = Tail{Offset: q.start, Line: q.lineNo, Before: before}
		e.emitted = true

		e.stats.Records++
		if !entry.Parsed {
			e.stats.Unparsed++
		}
		if !entry.HasTimestamp() {
			e.stats.NoTimestamp++
		} else if !entry.TimestampZoned {
			e.stats.ZoneAssumed++
			if entry.ZoneAbbrev != "" {
				if e.stats.ZoneAbbrevs == nil {
					e.stats.ZoneAbbrevs = map[string]int64{}
				}
				e.stats.ZoneAbbrevs[entry.ZoneAbbrev]++
			}
		}
		if err := e.fn(entry); err != nil {
			return err
		}
	}
	return nil
}

// parseRange runs work over 0..n-1 across at most workers goroutines.
//
// Contiguous slices rather than a work queue: every item is one line through
// one parser, so they cost about the same and the scheduling would cost more
// than the imbalance.
func parseRange(n, workers int, work func(int)) {
	if workers > n {
		workers = n
	}

	var wg sync.WaitGroup
	size := (n + workers - 1) / workers
	for start := 0; start < n; start += size {
		end := start + size
		if end > n {
			end = n
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			for i := lo; i < hi; i++ {
				work(i)
			}
		}(start, end)
	}
	wg.Wait()
}

// applyAssumedZone reinterprets a zoneless timestamp in the source's assumed
// timezone.
//
// Parsers resolve a timestamp that carries no offset as though it were UTC.
// That is a neutral carrier, not a claim: it lets a parser stay a pure function
// of one line, and keeps the assumed-zone question — which belongs to the
// source, not the format — out of the Parser interface, which is the
// contribution surface and must stay tiny.
//
// Reinterpreting means keeping the wall-clock reading and changing the zone,
// not shifting the instant. A line saying 14:00 in a source assumed to be
// Asia/Tokyo happened at 05:00 UTC, and time.Date with the target location is
// what the tz database gives us for that.
func applyAssumedZone(rec Record, loc *time.Location) Record {
	if rec.TimestampZoned || !rec.HasTimestamp() || loc == time.UTC || loc == nil {
		return rec
	}

	t := rec.Timestamp
	rec.Timestamp = time.Date(t.Year(), t.Month(), t.Day(),
		t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc)
	return rec
}

// readLine reads one line, returning it without its trailing newline.
//
// A line longer than MaxLineBytes is truncated and the remainder discarded, so
// that one pathological line cannot exhaust memory or stall the read. The
// returned flag lets the caller record that it happened.
//
// The final line of a file with no trailing newline is returned normally. That
// case is common — it is what a killed process leaves behind — and dropping it
// would be silent data loss.
//
// n is every byte taken from br, including the newline and anything discarded
// from an overlong line. The caller tracks stream position with it, so it must
// account for bytes that never reach the returned line.
func readLine(br *bufio.Reader) (line []byte, truncated bool, n int64, err error) {
	var buf []byte

	for {
		chunk, e := br.ReadSlice('\n')
		n += int64(len(chunk))

		if len(buf)+len(chunk) > MaxLineBytes {
			keep := MaxLineBytes - len(buf)
			if keep > 0 {
				buf = append(buf, chunk[:keep]...)
			}
			truncated = true
			if e == bufio.ErrBufferFull {
				discarded, derr := discardToNewline(br)
				n += discarded
				if derr != nil {
					return buf, truncated, n, derr
				}
			}
			return buf, truncated, n, nil
		}

		if e == bufio.ErrBufferFull {
			buf = append(buf, chunk...)
			continue
		}
		if e != nil {
			buf = append(buf, chunk...)
			return bytes.TrimSuffix(buf, []byte{'\n'}), truncated, n, e
		}

		buf = append(buf, chunk...)
		return bytes.TrimSuffix(buf, []byte{'\n'}), truncated, n, nil
	}
}

// discardToNewline throws away the rest of an overlong line, reporting how many
// bytes went with it so the caller's stream position stays accurate.
func discardToNewline(br *bufio.Reader) (n int64, err error) {
	for {
		chunk, err := br.ReadSlice('\n')
		n += int64(len(chunk))
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF {
			return n, nil
		}
		return n, err
	}
}

// Describe renders stats as a status line. Every non-zero count is shown,
// because the ones that look uninteresting are exactly the ones that mislead
// when omitted.
func (s Stats) Describe() string {
	parts := []string{fmt.Sprintf("%d records", s.Records)}

	if s.Unparsed > 0 {
		parts = append(parts, fmt.Sprintf("%d unparsed", s.Unparsed))
	}
	if s.NoTimestamp > 0 {
		parts = append(parts, fmt.Sprintf("%d without a timestamp", s.NoTimestamp))
	}
	if s.Truncated > 0 {
		parts = append(parts, fmt.Sprintf("%d truncated", s.Truncated))
	}
	if s.Continuation > 0 {
		parts = append(parts, fmt.Sprintf("%d continuation lines", s.Continuation))
	}
	return strings.Join(parts, " · ")
}

// Equal compares two Stats.
//
// It exists because Stats holds a map and so is not comparable with ==. A nil
// map and an empty one are the same answer to "did these two reads agree", so
// they are normalised before the comparison rather than being allowed to make
// an incremental read look different from a cold one.
func (s Stats) Equal(other Stats) bool {
	if len(s.ZoneAbbrevs) == 0 && len(other.ZoneAbbrevs) == 0 {
		s.ZoneAbbrevs, other.ZoneAbbrevs = nil, nil
	}
	return reflect.DeepEqual(s, other)
}

// Add accumulates stats across sources.
func (s *Stats) Add(other Stats) {
	s.Lines += other.Lines
	s.Records += other.Records
	s.Unparsed += other.Unparsed
	s.NoTimestamp += other.NoTimestamp
	s.Continuation += other.Continuation
	s.Truncated += other.Truncated
	s.Blank += other.Blank
	s.ZoneAssumed += other.ZoneAssumed
	s.InvalidUTF8 += other.InvalidUTF8
	for abbrev, n := range other.ZoneAbbrevs {
		if s.ZoneAbbrevs == nil {
			s.ZoneAbbrevs = map[string]int64{}
		}
		s.ZoneAbbrevs[abbrev] += n
	}
}
