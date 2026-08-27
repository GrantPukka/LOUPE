package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/GrantPukka/loupe/internal/session"
	"github.com/GrantPukka/loupe/internal/source"
	"github.com/GrantPukka/loupe/internal/store"
)

// statusLine reports what was loaded and every assumption made loading it.
//
// This prints before results, on stderr, so it survives being piped. Its job is
// to make the tool's assumptions auditable in one glance: which timezone the
// times are in, how many records were skipped or damaged, and which sources
// depend on a guessed zone. Nothing here is optional decoration — an omitted
// count is how a handoff misleads.
func statusLine(w io.Writer, s *session.Session) {
	// A stream has not been read yet when this prints, so there are no counts
	// to give. Printing the empty ones would say "0 records" about a pipe that
	// is about to deliver thousands. They are reported when the stream ends
	// instead, which is the first moment they are true.
	if !s.Streaming() {
		fmt.Fprintf(w, "%s · %s\n", describeSources(s.Load), s.Load.Stats.Describe())
	}

	// The display timezone, always, even when it is UTC. A user must never
	// have to guess whose clock they are reading.
	//
	// The offset is taken at the data's own instant rather than now, because
	// during the fortnight after a clock change those differ, and the offset
	// that matters is the one the records were written under.
	at := time.Now()
	if _, newest, _, err := s.DB.TimeRange(context.Background()); err == nil && !newest.IsZero() {
		at = newest
	}
	zone, offset := at.In(s.Loc).Zone()
	fmt.Fprintf(w, "Times shown in %s (%s, %s)\n", s.Loc, zone, formatOffset(offset))

	if s.Streaming() {
		// The rest of this reports what a finished read found. A stream has
		// not finished, so only what is already true is printed: the zone
		// above, and why nothing was cached.
		cacheLine(w, s)
		return
	}

	for _, a := range s.Load.AssumedZones() {
		// When the format wrote an abbreviation loupe could not resolve, name
		// it. "read as UTC (default)" on records that plainly say AEST is a
		// true statement that leaves the reader with nothing to do; naming the
		// abbreviation turns it into an instruction.
		fmt.Fprintf(w, "Note: %s has %d record(s) with no timezone in the format, read as %s (%s)%s\n",
			a.Source.Name, a.Records, a.Source.Zone, a.Source.ZoneSource, abbrevHint(a))
	}

	if n := s.Load.Stats.InvalidUTF8; n > 0 {
		// Said as its own sentence rather than as another count in the summary
		// line, because it is the one number here the reader has to act on: a
		// search for text inside those records will not match what they typed.
		was := "were"
		if n == 1 {
			was = "was"
		}
		// It names both halves on purpose. The old wording promised "the
		// original bytes are in the loupe_raw_hex field" and then offered only
		// the filter, which finds the record without ever showing the bytes —
		// and the field is deliberately kept out of the promoted columns, so
		// `SELECT loupe_raw_hex` does not resolve either. Recovering the bytes
		// is the entire reason they were kept, so the line that mentions them
		// says how.
		fmt.Fprintf(w, "Note: %s contained invalid UTF-8 and %s stored with replacement characters; "+
			"%s:* finds them and `loupe sql <dir> \"SELECT line_no, %s AS hex FROM logs WHERE %s IS NOT NULL\"` "+
			"reads the original bytes back as hex.\n",
			countOf(n, "record", "records"), was,
			store.RawHexField, store.RawHexExpr, store.RawHexExpr)
	}

	cacheLine(w, s)

	writeSkips(w, s.Walk.Skipped)

	for _, err := range s.Load.Errors {
		fmt.Fprintf(w, "Warning: %v\n", err)
	}
}

// abbrevHint names the unresolved zone abbreviations a source's records carried,
// and how to act on them.
//
// It says nothing when the format carried no zone at all, which is the ordinary
// case and where there is nothing more to tell.
func abbrevHint(a store.AssumedZone) string {
	if len(a.Abbrevs) == 0 {
		return ""
	}
	if a.Source.ZoneSource != store.ZoneFromDefault {
		return fmt.Sprintf("; the format writes %s", strings.Join(a.Abbrevs, ", "))
	}
	return fmt.Sprintf("; the format writes %s — pass --source-tz to place %s records exactly",
		strings.Join(a.Abbrevs, ", "), a.Source.Name)
}

// cacheLine reports whether the ingest was reused.
//
// A miss states its reason. Someone who expected the second run to be instant
// and got a full re-ingest is owed an explanation, and the commonest one — a
// log file that is still being written to — is not obvious.
func cacheLine(w io.Writer, s *session.Session) {
	switch {
	case s.CacheHit:
		// The stored duration is what the original ingest cost, not this run.
		// Saying which is the difference between a reassuring number and a
		// confusing one.
		fmt.Fprintf(w, "Reused a cached ingest — reading these files cost %s when the cache was built. "+
			"Pass --no-cache to re-read them.\n",
			s.Load.Took.Round(time.Millisecond))
	case s.HasStream():
		// "Re-read the log files" is wrong for a pipe: there are no files, and
		// nothing was re-read because nothing was ever stored. Saying so is
		// the point — somebody piping a large stream twice should know why it
		// costs full price both times.
		fmt.Fprintf(w, "Not cached: %s. A stream is gone once read.\n", s.CacheReason)
	case s.CacheReason != "":
		fmt.Fprintf(w, "Re-read the log files: %s\n", s.CacheReason)
	}

	// A directory with many sources can promote dozens of fields, and a status
	// line that wraps four times is one nobody reads. Name the most widely
	// covered and count the rest; `loupe sql "DESCRIBE logs"` has the full list.
	if len(s.Promoted) > 0 {
		const show = 6

		names := make([]string, 0, show)
		for _, p := range s.Promoted[:min(show, len(s.Promoted))] {
			names = append(names, fmt.Sprintf("%s (%s)", p.Field, p.Kind))
		}

		more := ""
		if len(s.Promoted) > show {
			more = fmt.Sprintf(", and %d more", len(s.Promoted)-show)
		}
		fmt.Fprintf(w, "Promoted %d field(s) to columns: %s%s\n",
			len(s.Promoted), strings.Join(names, ", "), more)
	}
}

// timeBanner prints the resolved window in both timezones, and every note the
// resolver produced.
//
// docs/FILTER-DSL.md section 2.3 calls this the feature rather than a nicety.
// Somebody working an incident at four in the morning should never have to do
// offset arithmetic, and the UTC line is what they paste into the ticket.
func timeBanner(w io.Writer, s *session.Session, plan session.Plan) {
	res := plan.Resolution

	if res.HasTimeFilter() {
		fmt.Fprintf(w, "Window: %s\n", res.Interval.Describe(s.Loc))
		for _, ex := range res.Exclude {
			fmt.Fprintf(w, "Excluding: %s\n", ex.Describe(s.Loc))
		}
	}

	for _, note := range res.Notes {
		fmt.Fprintf(w, "Note: %s\n", note.Text)
	}

	// A time filter necessarily excludes records with no timestamp. Silently
	// dropping them is a bug, so the count is always stated and the term that
	// finds them is offered.
	if res.HasTimeFilter() {
		if n := s.NoTimestamp(context.Background()); n > 0 {
			fmt.Fprintf(w, "%d record(s) excluded for having no timestamp — use ts:none to inspect them\n", n)
		}
	}
}

// repeatedSkip is how many files must share a reason before they are counted
// rather than listed.
//
// Below it, naming each file is what the reader wants. Above it, the list is
// the problem: a node walking /var/log finds every pod log twice, and three
// hundred lines saying so bury the status line the counts live in.
const repeatedSkip = 3

// writeSkips reports what the walk passed over, collapsing repetition.
//
// Never a bare silence and never a wall: every skipped file is counted, and the
// ones that share a reason are counted together.
func writeSkips(w io.Writer, skipped []source.Skip) {
	byReason := map[string]int{}
	for _, skip := range skipped {
		byReason[skip.Reason]++
	}

	// Reported in walk order, so the first file with each reason is where its
	// group appears and the output does not reorder itself between runs.
	done := map[string]bool{}
	for _, skip := range skipped {
		if done[skip.Reason] {
			continue
		}
		if n := byReason[skip.Reason]; n >= repeatedSkip {
			done[skip.Reason] = true
			fmt.Fprintf(w, "Skipped %d file(s): %s\n", n, skip.Reason)
			continue
		}
		fmt.Fprintf(w, "Skipped %s: %s\n", skip.Path, skip.Reason)
	}
}

func describeSources(load store.Load) string {
	sources := load.Sources()
	files := len(load.Results)

	switch {
	case files == 0:
		return "no sources"
	case len(sources) == files:
		return fmt.Sprintf("%d source(s)", files)
	default:
		// Rotated files collapse into one logical source, so say both numbers
		// rather than appearing to have lost files.
		return fmt.Sprintf("%d source(s) across %d file(s)", len(sources), files)
	}
}

// formatOffset renders a UTC offset the way a person writes it.
func formatOffset(seconds int) string {
	sign := "+"
	if seconds < 0 {
		sign, seconds = "-", -seconds
	}
	return fmt.Sprintf("UTC%s%02d:%02d", sign, seconds/3600, (seconds%3600)/60)
}
