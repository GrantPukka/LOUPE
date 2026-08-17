package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/GrantPukka/loupe/internal/session"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// page is how many records are fetched at a time.
const page = 500

// Run starts the interactive terminal interface.
//
// It is a third view over the same session the CLI and the HTTP API use, for
// the common case of being on a box with no browser and no way to copy a log
// directory off it. Anything reachable here is reachable from the CLI.
func Run(ctx context.Context, sess *session.Session, initialFilter string) error {
	m := newModel(ctx, sess, initialFilter)

	program := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := program.Run()
	return err
}

// focus is which part of the screen has the keyboard.
type focus int

const (
	focusList focus = iota
	focusFilter
)

type model struct {
	ctx  context.Context
	sess *session.Session

	filter textinput.Model
	focus  focus

	// applied is the filter the current results came from, which may lag the
	// input while the user is typing.
	applied string

	rows    [][]any
	columns map[string]int
	total   int64
	took    time.Duration
	hist    session.Histogram
	notes   []string
	window  string

	// cursor is the selected row, offset the first visible one.
	cursor int
	offset int

	// expanded is the seq of the open record, or -1.
	expanded int64
	detail   map[string]any

	err     error
	explain string
	loading bool
	showAll bool

	width, height int
}

func newModel(ctx context.Context, sess *session.Session, initialFilter string) model {
	input := textinput.New()
	input.Prompt = "› "
	input.Placeholder = "level:error   trace_id:a91c40f2   status:>=500   -source:nginx   last:15m"
	input.SetValue(initialFilter)
	input.CharLimit = 0

	return model{
		ctx:      ctx,
		sess:     sess,
		filter:   input,
		applied:  initialFilter,
		expanded: -1,
		loading:  true,
		width:    100,
		height:   30,
	}
}

func (m model) Init() tea.Cmd {
	return m.runQuery(m.applied)
}

// ---------------------------------------------------------------- messages

type resultMsg struct {
	filter  string
	rows    [][]any
	columns map[string]int
	total   int64
	took    time.Duration
	hist    session.Histogram
	notes   []string
	window  string
	explain string
}

type moreMsg struct {
	filter string
	rows   [][]any
}

type detailMsg struct {
	seq    int64
	record map[string]any
}

type errMsg struct {
	filter string
	err    error
}

// runQuery plans and runs a filter off the UI thread.
func (m model) runQuery(filter string) tea.Cmd {
	sess := m.sess
	ctx := m.ctx

	return func() tea.Msg {
		plan, err := sess.Plan(ctx, filter)
		if err != nil {
			return errMsg{filter: filter, err: err}
		}

		res, err := sess.Records(ctx, plan, session.RecordQuery{
			Limit:   page,
			Columns: "seq, " + session.RecordColumns,
		})
		if err != nil {
			return errMsg{filter: filter, err: err}
		}

		hist, err := sess.Histogram(ctx, plan, session.HistogramQuery{Buckets: 60})
		if err != nil {
			return errMsg{filter: filter, err: err}
		}

		msg := resultMsg{
			filter:  filter,
			rows:    res.Rows,
			columns: indexOf(res.Columns),
			total:   res.Total,
			took:    res.Took,
			hist:    hist,
		}

		for _, note := range plan.Resolution.Notes {
			msg.notes = append(msg.notes, note.Text)
		}
		if plan.Resolution.HasTimeFilter() {
			msg.window = plan.Resolution.Interval.Describe(sess.Loc)
		}
		if res.RowCount() == 0 && !plan.Query.IsEmpty() {
			msg.explain = sess.Explain(ctx, plan).Text
		}

		return msg
	}
}

func (m model) loadMore() tea.Cmd {
	sess, ctx, filter, offset := m.sess, m.ctx, m.applied, len(m.rows)

	return func() tea.Msg {
		plan, err := sess.Plan(ctx, filter)
		if err != nil {
			return errMsg{filter: filter, err: err}
		}

		res, err := sess.Records(ctx, plan, session.RecordQuery{
			Limit:   page,
			Offset:  offset,
			Columns: "seq, " + session.RecordColumns,
		})
		if err != nil {
			return errMsg{filter: filter, err: err}
		}
		return moreMsg{filter: filter, rows: res.Rows}
	}
}

func (m model) loadDetail(seq int64) tea.Cmd {
	sess, ctx := m.sess, m.ctx

	return func() tea.Msg {
		plan, err := sess.Plan(ctx, fmt.Sprintf("seq:%d", seq))
		if err != nil {
			return errMsg{err: err}
		}

		res, err := sess.Records(ctx, plan, session.RecordQuery{
			Limit:   1,
			Columns: "seq, ts, ts_zoned, level, message, source, file, format, line_no, parsed, raw, fields",
		})
		if err != nil || len(res.Rows) == 0 {
			return errMsg{err: err}
		}

		record := map[string]any{}
		for i, name := range res.Columns {
			record[name] = res.Rows[0][i]
		}
		return detailMsg{seq: seq, record: record}
	}
}

// ------------------------------------------------------------------ update

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case resultMsg:
		// A slow query for a filter the user has moved on from must not
		// overwrite the current results.
		if msg.filter != m.applied {
			return m, nil
		}
		m.rows, m.columns, m.total, m.took = msg.rows, msg.columns, msg.total, msg.took
		m.hist, m.notes, m.window, m.explain = msg.hist, msg.notes, msg.window, msg.explain
		m.cursor, m.offset, m.expanded, m.detail = 0, 0, -1, nil
		m.err, m.loading = nil, false
		return m, nil

	case moreMsg:
		if msg.filter != m.applied {
			return m, nil
		}
		m.rows = append(m.rows, msg.rows...)
		m.loading = false
		return m, nil

	case detailMsg:
		if msg.seq == m.expanded {
			m.detail = msg.record
		}
		return m, nil

	case errMsg:
		if msg.filter != "" && msg.filter != m.applied {
			return m, nil
		}
		m.err, m.loading = msg.err, false
		return m, nil

	case tea.KeyMsg:
		return m.onKey(msg)
	}

	return m, nil
}

func (m model) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.focus == focusFilter {
		return m.onFilterKey(msg)
	}
	return m.onListKey(msg)
}

func (m model) onFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.focus = focusList
		if value := strings.TrimSpace(m.filter.Value()); value != m.applied {
			m.applied, m.loading, m.err = value, true, nil
			return m, m.runQuery(value)
		}
		return m, nil

	case "esc":
		// Escape leaves the box without applying, so an abandoned edit does
		// not silently change what is on screen.
		m.focus = focusList
		m.filter.SetValue(m.applied)
		m.filter.Blur()
		return m, nil

	case "ctrl+c":
		return m, tea.Quit
	}

	var cmd tea.Cmd
	m.filter, cmd = m.filter.Update(msg)
	return m, cmd
}

func (m model) onListKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit

	case "/":
		m.focus = focusFilter
		m.filter.Focus()
		m.filter.CursorEnd()
		return m, nil

	case "esc":
		// Clear the filter from the list, which is the fastest way back to
		// everything.
		if m.applied != "" {
			m.filter.SetValue("")
			m.applied, m.loading, m.err = "", true, nil
			return m, m.runQuery("")
		}
		return m, nil

	case "j", "down":
		return m.move(1)
	case "k", "up":
		return m.move(-1)
	case "ctrl+d", "pgdown":
		return m.move(m.listHeight() / 2)
	case "ctrl+u", "pgup":
		return m.move(-m.listHeight() / 2)
	case "g", "home":
		m.cursor, m.offset = 0, 0
		return m, nil
	case "G", "end":
		return m.move(len(m.rows))

	case "enter", " ":
		return m.toggleDetail()

	case "a":
		// Show every column of the detail rather than the common ones.
		m.showAll = !m.showAll
		return m, nil

	case "f":
		// Filter by the selected row's source, which is the commonest
		// narrowing and saves typing it.
		return m.filterBySource()
	}

	return m, nil
}

func (m model) move(delta int) (tea.Model, tea.Cmd) {
	if len(m.rows) == 0 {
		return m, nil
	}

	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.rows) {
		m.cursor = len(m.rows) - 1
	}

	// Collapsing on move keeps the list predictable; an open record scrolling
	// past would leave the detail attached to nothing.
	if m.expanded >= 0 {
		m.expanded, m.detail = -1, nil
	}

	height := m.listHeight()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+height {
		m.offset = m.cursor - height + 1
	}

	// Fetch the next page before the end, so scrolling does not stall.
	if !m.loading && int64(len(m.rows)) < m.total && m.cursor > len(m.rows)-page/4 {
		m.loading = true
		return m, m.loadMore()
	}
	return m, nil
}

func (m model) toggleDetail() (tea.Model, tea.Cmd) {
	if len(m.rows) == 0 {
		return m, nil
	}

	seq := asInt(m.rows[m.cursor][m.columns["seq"]])
	if m.expanded == seq {
		m.expanded, m.detail = -1, nil
		return m, nil
	}

	m.expanded, m.detail = seq, nil
	return m, m.loadDetail(seq)
}

func (m model) filterBySource() (tea.Model, tea.Cmd) {
	if len(m.rows) == 0 {
		return m, nil
	}

	source, _ := m.rows[m.cursor][m.columns["source"]].(string)
	if source == "" {
		return m, nil
	}

	// A real DSL term, put in the box where the user can see and edit it —
	// the same principle as the web UI's click-to-filter.
	next := strings.TrimSpace(m.applied + " source:" + source)
	m.filter.SetValue(next)
	m.applied, m.loading, m.err = next, true, nil
	return m, m.runQuery(next)
}

func indexOf(columns []string) map[string]int {
	out := make(map[string]int, len(columns))
	for i, name := range columns {
		out[name] = i
	}
	return out
}

func asInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int:
		return int64(n)
	default:
		return -1
	}
}
