package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/user"
	"sort"
	"strings"
	"time"

	"github.com/VIGIL-OPS/loupe/internal/store"
)

// DefaultHandoffLimit caps the record table.
//
// docs/HANDOFF.md section 3: cap at 200 rows by default, and always state when
// output was truncated and what the full count was.
const DefaultHandoffLimit = 200

// HandoffOptions configures an extract.
type HandoffOptions struct {
	// Limit caps the records included. Zero means DefaultHandoffLimit.
	Limit int

	// Redact replaces the values of these fields with a stable hash.
	//
	// Opt-in and explicit, never on by default: silently altering evidence is
	// worse than exposing it, and the operator knows their disclosure rules
	// better than the tool does.
	Redact []string

	// Version and Command describe the invocation, for provenance.
	Version string
	Command string
}

func (o HandoffOptions) limit() int {
	if o.Limit > 0 {
		return o.Limit
	}
	return DefaultHandoffLimit
}

// Handoff is everything a receiving engineer needs to reproduce and trust a
// finding without a conversation.
//
// Ordered by what the reader reads first, per docs/HANDOFF.md section 2.
type Handoff struct {
	// Query is the filter exactly as written, so it can be re-run verbatim.
	Query string `json:"query"`

	// Window is the resolved range in both timezones.
	WindowLocal string    `json:"window_local,omitempty"`
	WindowUTC   string    `json:"window_utc,omitempty"`
	WindowStart time.Time `json:"window_start,omitempty"`
	WindowEnd   time.Time `json:"window_end,omitempty"`
	Timezone    string    `json:"timezone"`

	// Notes are the assumptions the resolver made — a chosen date, an anchor,
	// a clock change inside the window.
	Notes []string `json:"notes,omitempty"`

	Sources []HandoffSource `json:"sources"`
	Counts  HandoffCounts   `json:"counts"`
	Records []HandoffRecord `json:"records"`

	// Truncated says the record table is not the whole match set. An extract
	// that does not admit it is truncated is worse than no extract.
	Truncated bool `json:"truncated"`

	Redacted []string          `json:"redacted,omitempty"`
	Meta     HandoffProvenance `json:"generated"`
}

// HandoffSource is one file the records came from.
type HandoffSource struct {
	File   string `json:"file"`
	Format string `json:"format"`
	Bytes  int64  `json:"bytes"`
	// Records is how many the file contributed to the whole ingest, not to the
	// match: a reader checking the numbers needs the denominator.
	Records int64 `json:"records"`
	// Timezone is known or assumed. A receiver who does not know an assumption
	// was made cannot check it.
	Timezone    string `json:"timezone"`
	Unparsed    int64  `json:"unparsed"`
	NoTimestamp int64  `json:"no_timestamp"`
}

// HandoffCounts is what was included and what was left out.
//
// Silence about excluded data is how handoffs mislead, so every figure appears
// whether or not it is interesting.
type HandoffCounts struct {
	Matched int64 `json:"matched"`
	Shown   int64 `json:"shown"`
	// ExcludedNoTimestamp is how many records a time filter left out.
	ExcludedNoTimestamp int64 `json:"excluded_no_timestamp"`
	// Unparsed is how many lines no parser understood, across the ingest.
	Unparsed int64 `json:"unparsed"`
	// Ingested is the total the extract was drawn from.
	Ingested int64 `json:"ingested"`
}

// HandoffRecord is one matched record, in both timezones, with its raw line.
type HandoffRecord struct {
	Local  string         `json:"local"`
	UTC    string         `json:"utc"`
	Level  string         `json:"level"`
	Source string         `json:"source"`
	Text   string         `json:"message"`
	Fields map[string]any `json:"fields,omitempty"`
	// Raw is the original line, verbatim. The receiver may not trust our
	// parser, and they are right not to.
	Raw string `json:"raw"`
	// ZoneAssumed marks a timestamp that depends on --source-tz.
	ZoneAssumed bool `json:"zone_assumed,omitempty"`
	// Parsed is false for a line no parser understood.
	Parsed bool `json:"parsed"`
}

// HandoffProvenance says who produced the extract and how.
type HandoffProvenance struct {
	Tool    string    `json:"tool"`
	Version string    `json:"version"`
	Host    string    `json:"host"`
	User    string    `json:"user"`
	At      time.Time `json:"at"`
	Command string    `json:"command,omitempty"`
	Path    string    `json:"path,omitempty"`
}

// Handoff builds an extract from a plan.
//
// It runs the same plan the display path did. It must never be possible for the
// exported records to differ from what was on screen: same AST, same SQL,
// different renderer.
func (s *Session) Handoff(ctx context.Context, plan Plan, opts HandoffOptions) (Handoff, error) {
	out := Handoff{
		Query:    plan.Filter,
		Timezone: s.Loc.String(),
		Redacted: opts.Redact,
		Meta:     provenance(opts),
	}

	if plan.Resolution.HasTimeFilter() {
		interval := plan.Resolution.Interval
		out.WindowStart, out.WindowEnd = interval.Start, interval.End
		// Two readings of the same window: the one the operator saw, and the
		// one the receiver will paste into a ticket. Nobody should have to do
		// offset arithmetic at either end.
		out.WindowLocal = interval.DescribeZone(s.Loc)
		out.WindowUTC = interval.DescribeZone(time.UTC)
	}
	for _, note := range plan.Resolution.Notes {
		out.Notes = append(out.Notes, note.Text)
	}

	matched, err := s.Count(ctx, plan)
	if err != nil {
		return Handoff{}, err
	}

	out.Counts = HandoffCounts{
		Matched:  matched,
		Unparsed: s.Load.Stats.Unparsed,
		Ingested: s.Load.Stats.Records,
	}
	if plan.Resolution.HasTimeFilter() {
		out.Counts.ExcludedNoTimestamp = s.NoTimestamp(ctx)
	}

	if err := s.handoffSources(ctx, &out); err != nil {
		return Handoff{}, err
	}
	if err := s.handoffRecords(ctx, plan, opts, &out); err != nil {
		return Handoff{}, err
	}

	return out, nil
}

func (s *Session) handoffSources(ctx context.Context, out *Handoff) error {
	infos, err := s.DB.Sources(ctx)
	if err != nil {
		return err
	}

	for _, info := range infos {
		source := HandoffSource{
			File:        info.File,
			Format:      info.Format,
			Records:     info.Records,
			Timezone:    info.TimezoneStatus(),
			Unparsed:    info.Unparsed,
			NoTimestamp: info.NoTimestamp,
		}
		// Size is read from disk rather than stored, so a file that changed
		// since ingest shows its current size and the mismatch is visible.
		if stat, err := os.Stat(info.File); err == nil {
			source.Bytes = stat.Size()
		}
		out.Sources = append(out.Sources, source)
	}
	return nil
}

func (s *Session) handoffRecords(ctx context.Context, plan Plan, opts HandoffOptions, out *Handoff) error {
	res, err := s.Records(ctx, plan, RecordQuery{
		Limit:   opts.limit(),
		Columns: "ts, ts_zoned, level, message, source, parsed, raw, fields",
	})
	if err != nil {
		return err
	}

	out.Truncated = res.Truncated
	out.Counts.Shown = int64(res.RowCount())

	index := map[string]int{}
	for i, name := range res.Columns {
		index[name] = i
	}

	redact := map[string]bool{}
	for _, field := range opts.Redact {
		redact[strings.ToLower(field)] = true
	}

	for _, row := range res.Rows {
		record := HandoffRecord{
			Level:  text(row[index["level"]]),
			Source: text(row[index["source"]]),
			Text:   text(row[index["message"]]),
			Raw:    text(row[index["raw"]]),
		}
		if parsed, ok := row[index["parsed"]].(bool); ok {
			record.Parsed = parsed
		}
		if zoned, ok := row[index["ts_zoned"]].(bool); ok {
			record.ZoneAssumed = !zoned
		}

		if ts, ok := row[index["ts"]].(time.Time); ok {
			record.Local = ts.In(s.Loc).Format("2006-01-02 15:04:05.000")
			record.UTC = ts.UTC().Format("2006-01-02 15:04:05.000")
		}

		record.Fields = decodeFields(text(row[index["fields"]]))

		if len(redact) > 0 {
			applyRedaction(&record, redact)
		}
		out.Records = append(out.Records, record)
	}

	return nil
}

// applyRedaction replaces named field values with a stable hash.
//
// Stable so records can still be correlated — the same user id hashes to the
// same token throughout the extract — without exposing the value.
//
// The raw line is redacted too. Leaving the value visible there while masking
// the field would be a redaction that does not redact.
func applyRedaction(record *HandoffRecord, redact map[string]bool) {
	for key, value := range record.Fields {
		if !redact[strings.ToLower(key)] {
			continue
		}

		original := text(value)
		token := redactionToken(original)
		record.Fields[key] = token

		if original != "" {
			record.Raw = strings.ReplaceAll(record.Raw, original, token)
			record.Text = strings.ReplaceAll(record.Text, original, token)
		}
	}
}

// redactionToken is a short stable hash of a value.
func redactionToken(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return "redacted:" + hex.EncodeToString(sum[:])[:12]
}

func provenance(opts HandoffOptions) HandoffProvenance {
	meta := HandoffProvenance{
		Tool:    "loupe",
		Version: opts.Version,
		At:      time.Now().UTC(),
		Command: opts.Command,
	}
	if host, err := os.Hostname(); err == nil {
		meta.Host = host
	}
	if u, err := user.Current(); err == nil {
		meta.User = u.Username
	}
	if wd, err := os.Getwd(); err == nil {
		meta.Path = wd
	}
	return meta
}

// AssumedSources lists the sources in an extract whose timezone was assumed.
func (h Handoff) AssumedSources() []HandoffSource {
	var out []HandoffSource
	for _, s := range h.Sources {
		if !strings.HasPrefix(s.Timezone, "known") {
			out = append(out, s)
		}
	}
	return out
}

func decodeFields(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	out, err := store.DecodeFields(raw)
	if err != nil {
		// An undecodable bag is still evidence, so it travels as text.
		return map[string]any{"_raw_fields": raw}
	}
	return out
}

func text(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	case time.Time:
		return t.Format(time.RFC3339Nano)
	default:
		return fmt.Sprint(t)
	}
}

// SortedFieldNames returns a record's field keys in a stable order, so two
// extracts of the same data are byte-identical.
func SortedFieldNames(fields map[string]any) []string {
	out := make([]string, 0, len(fields))
	for k := range fields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
