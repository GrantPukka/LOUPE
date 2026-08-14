package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/VIGIL-OPS/loupe/internal/store"
)

// statusLine reports what was loaded and every assumption made loading it.
//
// This prints before results, on stderr, so it survives being piped. Its job is
// to make the tool's assumptions auditable in one glance: which timezone the
// times are in, how many records were skipped or damaged, and which sources
// depend on a guessed zone. Nothing here is optional decoration — an omitted
// count is how a handoff misleads.
func (s *session) statusLine(w io.Writer) {
	stats := s.load.Stats

	fmt.Fprintf(w, "%s · %s\n",
		describeSources(s.load),
		stats.Describe())

	// The display timezone, always, even when it is UTC. A user must never
	// have to guess whose clock they are reading.
	//
	// The offset is taken at the data's own instant rather than now, because
	// during the fortnight after a clock change those differ, and the offset
	// that matters is the one the records were written under.
	at := time.Now()
	if newest := s.newestRecord(); !newest.IsZero() {
		at = newest
	}
	zone, offset := at.In(s.loc).Zone()
	fmt.Fprintf(w, "Times shown in %s (%s, %s)\n", s.loc, zone, formatOffset(offset))

	for _, a := range s.load.AssumedZones() {
		fmt.Fprintf(w, "Note: %s has %d record(s) with no timezone in the format, read as %s (%s)\n",
			a.Source.Name, a.Records, a.Source.Zone, a.Source.ZoneSource)
	}

	s.cacheLine(w)

	for _, skip := range s.walk.Skipped {
		fmt.Fprintf(w, "Skipped %s: %s\n", skip.Path, skip.Reason)
	}

	for _, err := range s.load.Errors {
		fmt.Fprintf(w, "Warning: %v\n", err)
	}
}

// cacheLine reports whether the ingest was reused.
//
// A miss states its reason. Someone who expected the second run to be instant
// and got a full re-ingest is owed an explanation, and the commonest one — a
// log file that is still being written to — is not obvious.
func (s *session) cacheLine(w io.Writer) {
	switch {
	case s.cacheHit:
		// The stored duration is what the original ingest cost, not this run.
		// Saying which is the difference between a reassuring number and a
		// confusing one.
		fmt.Fprintf(w, "Reused a cached ingest — the original read took %s. Pass --no-cache to re-read the files.\n",
			s.load.Took.Round(time.Millisecond))
	case s.cacheReason != "":
		fmt.Fprintf(w, "Re-read the log files: %s\n", s.cacheReason)
	}

	// A directory with many sources can promote dozens of fields, and a status
	// line that wraps four times is one nobody reads. Name the most widely
	// covered and count the rest; `loupe sql "DESCRIBE logs"` has the full list.
	if len(s.promoted) > 0 {
		const show = 6

		names := make([]string, 0, show)
		for _, p := range s.promoted[:min(show, len(s.promoted))] {
			names = append(names, fmt.Sprintf("%s (%s)", p.Field, p.Kind))
		}

		more := ""
		if len(s.promoted) > show {
			more = fmt.Sprintf(", and %d more", len(s.promoted)-show)
		}
		fmt.Fprintf(w, "Promoted %d field(s) to columns: %s%s\n",
			len(s.promoted), strings.Join(names, ", "), more)
	}
}

// newestRecord is the latest timestamp in the loaded data, or the zero time
// when nothing carried one.
func (s *session) newestRecord() time.Time {
	_, newest, _, err := s.db.TimeRange(context.Background())
	if err != nil {
		return time.Time{}
	}
	return newest
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
