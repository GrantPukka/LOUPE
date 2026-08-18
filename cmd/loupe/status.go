package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/GrantPukka/loupe/internal/session"
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
		fmt.Fprintf(w, "Note: %s has %d record(s) with no timezone in the format, read as %s (%s)\n",
			a.Source.Name, a.Records, a.Source.Zone, a.Source.ZoneSource)
	}

	cacheLine(w, s)

	for _, skip := range s.Walk.Skipped {
		fmt.Fprintf(w, "Skipped %s: %s\n", skip.Path, skip.Reason)
	}

	for _, err := range s.Load.Errors {
		fmt.Fprintf(w, "Warning: %v\n", err)
	}
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
		fmt.Fprintf(w, "Reused a cached ingest — the original read took %s. Pass --no-cache to re-read the files.\n",
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
