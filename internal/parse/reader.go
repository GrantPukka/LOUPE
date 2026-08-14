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

// ReadAll streams r through the parser, calling fn for each record.
//
// The governing rule: a malformed line never aborts a file. Anything the parser
// rejects becomes an Entry with Parsed false and its raw text intact, and
// reading continues.
//
// It streams. Memory stays bounded regardless of input size.
func ReadAll(r io.Reader, opts ReaderOptions, fn func(Entry) error) (Stats, error) {
	var stats Stats

	if opts.Parser == nil {
		return stats, fmt.Errorf("read: no parser given")
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

	flush := func() error {
		if pending == nil {
			return nil
		}
		e := *pending
		pending = nil

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
		line, truncated, err := readLine(br)
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

		if ferr := flush(); ferr != nil {
			return stats, ferr
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

		if err == io.EOF {
			break
		}
		if err != nil {
			flushErr := flush()
			if flushErr != nil {
				return stats, flushErr
			}
			return stats, fmt.Errorf("read line %d: %w", lineNo, err)
		}
	}

	if err := flush(); err != nil {
		return stats, err
	}
	return stats, nil
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
func readLine(br *bufio.Reader) (line []byte, truncated bool, err error) {
	var buf []byte

	for {
		chunk, e := br.ReadSlice('\n')

		if len(buf)+len(chunk) > MaxLineBytes {
			keep := MaxLineBytes - len(buf)
			if keep > 0 {
				buf = append(buf, chunk[:keep]...)
			}
			truncated = true
			if e == bufio.ErrBufferFull {
				if derr := discardToNewline(br); derr != nil {
					return buf, truncated, derr
				}
			}
			return buf, truncated, nil
		}

		if e == bufio.ErrBufferFull {
			buf = append(buf, chunk...)
			continue
		}
		if e != nil {
			buf = append(buf, chunk...)
			return bytes.TrimSuffix(buf, []byte{'\n'}), truncated, e
		}

		buf = append(buf, chunk...)
		return bytes.TrimSuffix(buf, []byte{'\n'}), truncated, nil
	}
}

// discardToNewline throws away the rest of an overlong line.
func discardToNewline(br *bufio.Reader) error {
	for {
		_, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF {
			return nil
		}
		return err
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
}
