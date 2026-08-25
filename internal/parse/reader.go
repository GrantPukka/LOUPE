package parse

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
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
}

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

	// pending holds the record being accumulated, so that continuation lines
	// can be appended to it before it is emitted.
	var pending *Entry

	// consumed counts every byte taken from r. pendingStart and pendingLine
	// record where the pending record began, and become the Tail once it is
	// emitted — so the Tail always describes the most recent record.
	var consumed, pendingStart, pendingLine int64
	var pendingBefore Stats
	emitted := false

	flush := func() error {
		if pending == nil {
			return nil
		}
		e := *pending
		pending = nil
		tail = Tail{Offset: pendingStart, Line: pendingLine, Before: pendingBefore}
		emitted = true

		stats.Records++
		if !e.Parsed {
			stats.Unparsed++
		}
		if !e.HasTimestamp() {
			stats.NoTimestamp++
		} else if !e.TimestampZoned {
			stats.ZoneAssumed++
		}
		return fn(e)
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
		if pending != nil && continuer != nil && continuer.IsContinuation(trimmed) {
			stats.Continuation++
			pending.Message += "\n" + string(trimmed)
			pending.Raw += "\n" + string(trimmed)
			if err == io.EOF {
				break
			}
			continue
		}

		// This line starts a new record, so the previous one is complete. flush
		// reads pendingStart, which still refers to that previous record.
		if ferr := flush(); ferr != nil {
			return stats, tail, ferr
		}

		entry := Entry{
			LineNo:    lineNo,
			Raw:       string(trimmed),
			Truncated: truncated,
		}

		rec, perr := opts.Parser.Parse(trimmed)
		if perr != nil {
			// Not an error worth propagating: keep the raw text and move on.
			entry.Record = Record{Message: string(trimmed), Fields: map[string]any{}}
		} else {
			entry.Record = applyAssumedZone(rec, loc)
			entry.Parsed = true
			if entry.Fields == nil {
				entry.Fields = map[string]any{}
			}
		}

		pending = &entry
		pendingStart = lineStart
		pendingLine = lineNo

		// The totals as they stood before this record. Taken after the flush
		// above, so the previous record is counted, then backing out this line's
		// own accounting — a resumed read starts at this line and counts it
		// again. Records is not adjusted: this record has not been flushed yet.
		pendingBefore = stats
		pendingBefore.Lines--
		if truncated {
			pendingBefore.Truncated--
		}

		// A format with no continuation lines has a complete record the moment
		// its line is read, so holding it back until the next one arrives buys
		// nothing — and on a stream it costs everything. `kubectl logs -f` on a
		// service that logs once a minute would always be showing the record
		// before last, which reads as the tail being stuck.
		//
		// Log4j and anything else that can continue still waits, because a
		// stack trace emitted before its own trace would be worse than late.
		if continuer == nil {
			if ferr := flush(); ferr != nil {
				return stats, tail, ferr
			}
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			flushErr := flush()
			if flushErr != nil {
				return stats, tail, flushErr
			}
			return stats, tail, fmt.Errorf("read line %d: %w", lineNo, err)
		}
	}

	if err := flush(); err != nil {
		return stats, tail, err
	}

	// Nothing was emitted, so there is no record to re-read. Resume past what
	// was consumed — blank lines and nothing else — rather than at zero, which
	// would re-count them on every pass.
	if !emitted {
		tail = Tail{Offset: consumed, Line: lineNo + 1, Before: stats}
	}
	return stats, tail, nil
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
}
