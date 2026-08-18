package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/GrantPukka/loupe/internal/session"
	"github.com/charmbracelet/lipgloss"
)

// Fixed chrome: header, filter, histogram, column header, footer, and the
// blank line under the histogram.
const chromeLines = 7

// detailLines is how much of the screen an expanded record may take.
const detailLines = 14

func (m model) listHeight() int {
	height := m.height - chromeLines
	if m.expanded >= 0 {
		height -= detailLines
	}
	if height < 3 {
		return 3
	}
	return height
}

func (m model) View() string {
	var b strings.Builder

	b.WriteString(m.header())
	b.WriteByte('\n')
	b.WriteString(m.filterLine())
	b.WriteByte('\n')
	b.WriteString(m.histogram())
	b.WriteByte('\n')
	b.WriteString(m.columnHeader())
	b.WriteByte('\n')
	b.WriteString(m.list())
	b.WriteString(m.footer())

	return b.String()
}

func (m model) header() string {
	stats := m.sess.Load.Stats

	sources := len(m.sess.Load.Sources())
	left := styleBrand.Render("loupe") + " " +
		styleGhost.Render(fmt.Sprintf("%d %s · %s records",
			sources, plural(sources, "source", "sources"), commas(stats.Records)))

	// The display timezone is never hidden. A user must never have to guess
	// whether the times on screen are theirs or the server's.
	right := styleSteel.Render(m.sess.Loc.String())

	return styleHeader.Width(m.width).Render(pad(left, right, m.width))
}

func (m model) filterLine() string {
	if m.focus == focusFilter {
		return m.filter.View()
	}

	value := m.applied
	if value == "" {
		return styleGhost.Render("› press / to filter")
	}
	return styleSteel.Render("› ") + styleVal.Render(value)
}

// blocks are the eighth-height glyphs used to draw a bar in one text row.
var blocks = []rune{' ', '▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// histogram draws the timeline as a single line of block characters.
//
// One line rather than the browser's fifty pixels: a terminal has rows to
// spare only in the record list, and the job here is the same — make a cluster
// visible without reading numbers.
func (m model) histogram() string {
	if m.err != nil {
		// Truncate to the first line; the full text goes in the list area.
		first, _, _ := strings.Cut(m.err.Error(), "\n")
		return styleError.Render("✗ " + truncate(first, m.width-2))
	}

	if len(m.hist.Buckets) == 0 {
		return styleGhost.Render(strings.Repeat("·", max(0, min(m.width, 60))))
	}

	max64 := m.hist.Max
	if max64 < 1 {
		max64 = 1
	}

	var bar strings.Builder
	for _, bucket := range m.hist.Buckets {
		level := worstLevel(bucket)
		glyph := blocks[scale(bucket.Count, max64)]
		bar.WriteString(levelStyle(level).Render(string(glyph)))
	}

	label := fmt.Sprintf(" %s → %s",
		clockOf(m.hist.Start, m.sess.Loc), clockOf(m.hist.End, m.sess.Loc))

	return bar.String() + styleGhost.Render(label)
}

// scale maps a count onto the eight block heights.
//
// A bucket holding records never renders as blank: against a busy peak a few
// hundred records round to zero, and a quiet period and a real cluster would
// then look identical.
func scale(count, max int64) int {
	if count == 0 {
		return 0
	}
	step := int(count * 8 / max)
	if step < 1 {
		return 1
	}
	if step > 8 {
		return 8
	}
	return step
}

func worstLevel(b session.Bucket) string {
	for _, level := range []string{"fatal", "error", "warn", "info", "debug", "trace"} {
		if b.Levels[level] > 0 {
			return level
		}
	}
	return ""
}

func (m model) columnHeader() string {
	head := fmt.Sprintf("%-12s %-6s %-16s %s", "TIME", "LEVEL", "SOURCE", "MESSAGE")
	return styleFooter.Width(m.width).Render(truncate(head, m.width))
}

func (m model) list() string {
	height := m.listHeight()

	if m.err != nil {
		return m.errorPane(height)
	}
	if len(m.rows) == 0 {
		message := m.explain
		if message == "" {
			message = "No records matched."
		}
		if m.loading {
			message = "loading…"
		}
		return blankFill(styleDim.Render("  "+message), height)
	}

	var b strings.Builder
	shown := 0

	for i := m.offset; i < len(m.rows) && shown < height; i++ {
		b.WriteString(m.renderRow(i))
		b.WriteByte('\n')
		shown++

		if m.expanded >= 0 && i == m.cursor {
			b.WriteString(m.detailPane())
		}
	}

	for ; shown < height; shown++ {
		b.WriteByte('\n')
	}
	return b.String()
}

func (m model) renderRow(i int) string {
	row := m.rows[i]

	// Exactly as wide as a rendered time, so a record with none does not push
	// every column after it out of line.
	stamp := "no timestamp"
	if ts, ok := row[m.columns["ts"]].(time.Time); ok {
		stamp = ts.In(m.sess.Loc).Format("15:04:05.000")
	}

	level, _ := row[m.columns["level"]].(string)
	source, _ := row[m.columns["source"]].(string)
	message, _ := row[m.columns["message"]].(string)

	// A multi-line record shows its first line and says how many more, so a
	// stack trace does not swamp the list.
	extra := strings.Count(message, "\n")
	if extra > 0 {
		message, _, _ = strings.Cut(message, "\n")
		message += fmt.Sprintf("  +%d lines", extra)
	}
	message = sanitise(message)

	line := fmt.Sprintf("%-12s %-6s %-16s %s",
		stamp, level, truncate(source, 16), message)
	line = truncate(line, m.width)

	if i == m.cursor {
		return styleSelected.Render(padRight(line, m.width))
	}
	return levelStyle(level).Render(line)
}

// detailPane renders the expanded record.
//
// The raw line is always included, in its native format. A reader may not
// trust our parser, and they are right not to.
func (m model) detailPane() string {
	if m.detail == nil {
		return styleGhost.Render("    loading…") + "\n"
	}

	var b strings.Builder
	write := func(key string, value any) {
		b.WriteString("    " + styleKey.Render(fmt.Sprintf("%-12s", key)) + " " +
			styleVal.Render(truncate(display(value), m.width-18)) + "\n")
	}

	// An assumed timezone changes what the timestamp means, so it is stated
	// before the value it affects.
	if zoned, ok := m.detail["ts_zoned"].(bool); ok && !zoned {
		if m.detail["ts"] != nil {
			b.WriteString("    " + styleWarn.Render(
				"timezone assumed — this format carries no offset (see --source-tz)") + "\n")
		}
	}
	if parsed, ok := m.detail["parsed"].(bool); ok && !parsed {
		b.WriteString("    " + styleWarn.Render(
			"unparsed — no parser matched this line, so only its raw text is available") + "\n")
	}

	for _, key := range []string{"source", "file", "format", "line_no"} {
		write(key, m.detail[key])
	}

	for _, key := range sortedKeys(m.fields()) {
		write(key, m.fields()[key])
	}

	b.WriteString("    " + styleGhost.Render(fmt.Sprintf("raw · %v · %v",
		m.detail["format"], m.detail["file"])) + "\n")

	for _, line := range strings.Split(display(m.detail["raw"]), "\n") {
		b.WriteString("    " + styleRaw.Render(truncate(sanitise(line), m.width-6)) + "\n")
	}

	return b.String()
}

func (m model) fields() map[string]any {
	raw, ok := m.detail["fields"].(string)
	if !ok || raw == "" {
		return nil
	}

	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return map[string]any{"fields": raw}
	}
	return out
}

func (m model) errorPane(height int) string {
	var b strings.Builder
	for _, line := range strings.Split(m.err.Error(), "\n") {
		b.WriteString("  " + styleError.Render(truncate(line, m.width-2)) + "\n")
	}
	return blankFill(b.String(), height)
}

func (m model) footer() string {
	if m.err != nil {
		return styleFooter.Width(m.width).Render(
			truncate("  / filter · esc clear · q quit", m.width))
	}

	left := fmt.Sprintf("  %s of %s records",
		commas(int64(len(m.rows))), commas(m.total))
	if m.took > 0 {
		left += fmt.Sprintf(" · %s", m.took.Round(time.Millisecond))
	}

	// Every exclusion is declared. A count omitted here is a count nobody
	// knows about.
	if m.hist.NoTimestamp > 0 {
		left += fmt.Sprintf(" · %s not on the timeline", commas(m.hist.NoTimestamp))
	}
	if m.window != "" {
		left += " · " + m.window
	}
	for _, note := range m.notes {
		left += " · " + note
	}

	if m.follower != nil {
		left += " · following"
		// What arrived below the cursor while it was elsewhere. Stated rather
		// than scrolled to, so nobody has to wonder whether the tail stalled
		// or they simply were not looking at it.
		if m.unseen > 0 {
			left += fmt.Sprintf(" · %s new below (G)", commas(int64(m.unseen)))
		}
	}
	// A source that stopped being readable mid-incident. The other sources are
	// still streaming, and this says which one is not.
	for _, note := range m.notices {
		left += " · " + note
	}

	right := "/ filter · j/k move · enter expand · f source · esc clear · q quit  "
	return styleFooter.Width(m.width).Render(truncate(pad(left, right, m.width), m.width))
}

// ------------------------------------------------------------------ helpers

func pad(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

func padRight(s string, width int) string {
	if gap := width - lipgloss.Width(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

func truncate(s string, width int) string {
	if width < 1 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[:width-1]) + "…"
}

func blankFill(s string, height int) string {
	lines := strings.Count(s, "\n")
	if lines < height {
		s += strings.Repeat("\n", height-lines)
	}
	return s
}

// sanitise escapes control characters rather than dropping them. A NUL byte
// from an interleaved write is evidence about what happened to the log.
func sanitise(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return s
	}

	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteString("    ")
		case r == 0:
			b.WriteString("\\0")
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func display(v any) string {
	switch t := v.(type) {
	case nil:
		return "—"
	case time.Time:
		return t.Format(time.RFC3339Nano)
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func clockOf(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "—"
	}
	return t.In(loc).Format("15:04:05")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func commas(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}

	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
