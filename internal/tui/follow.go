package tui

import (
	"time"

	"github.com/GrantPukka/loupe/internal/session"
	"github.com/GrantPukka/loupe/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

// tickMsg asks for a poll. liveMsg carries what one found.
type (
	tickMsg struct{}

	liveMsg struct {
		// filter is what was applied when the poll ran. A batch fetched under
		// a filter the user has since changed is discarded rather than
		// appended: the records are in the store, and the new filter's own
		// query will pick them up.
		filter string
		rows   [][]any
		// notices are per-source read failures. A file that became unreadable
		// mid-incident is shown, never swallowed.
		notices []string
	}
)

// tick schedules the next poll.
//
// The next tick is only ever scheduled once the current poll has returned, so
// two polls can never overlap. A Follower rewinds to the start of the last
// record and re-reads it, so two running at once would each undo the other's
// progress.
func tick() tea.Cmd {
	return tea.Tick(store.PollInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

// poll reads whatever has been appended and runs it through the applied
// filter.
//
// Live records go through the same compiled DSL as everything else. A tail
// that matched differently from the same filter run afterwards would disagree
// with itself, which is worse than not having one.
func (m model) poll() tea.Cmd {
	sess, ctx, filter, follower := m.sess, m.ctx, m.applied, m.follower

	return func() tea.Msg {
		batch, err := follower.Poll(ctx)
		if err != nil {
			return liveMsg{filter: filter, notices: []string{err.Error()}}
		}

		msg := liveMsg{filter: filter}
		for _, e := range batch.Errors {
			msg.notices = append(msg.notices, e.Error())
		}
		if batch.Records == 0 {
			return msg
		}

		plan, err := sess.Plan(ctx, filter)
		if err != nil {
			// The filter on screen already produced results, so this can only
			// be a transient failure. Report it and keep following.
			msg.notices = append(msg.notices, err.Error())
			return msg
		}

		where, args := batch.Predicate()
		res, err := sess.Records(ctx, plan, session.RecordQuery{
			Sort:      session.SortTime,
			Columns:   "seq, " + session.RecordColumns,
			Where:     where,
			WhereArgs: args,
		})
		if err != nil {
			msg.notices = append(msg.notices, err.Error())
			return msg
		}

		msg.rows = res.Rows
		return msg
	}
}

// onLive appends what arrived, and decides whether to follow it down.
//
// The list is oldest-first, so arrivals go on the end. The cursor moves with
// them only if it was already on the last row: someone reading further up is
// in the middle of something, and yanking them to the bottom every time a line
// lands makes the view unusable during exactly the incident they opened it
// for. The footer says how many they have not looked at, and G goes there.
func (m model) onLive(msg liveMsg) (tea.Model, tea.Cmd) {
	m.notices = mergeNotices(m.notices, msg.notices)

	if msg.filter != m.applied || len(msg.rows) == 0 {
		return m, tick()
	}

	following := m.cursor >= len(m.rows)-1
	m.rows = append(m.rows, msg.rows...)
	m.total += int64(len(msg.rows))

	if !following {
		m.unseen += len(msg.rows)
		return m, tick()
	}

	m.unseen = 0
	m.cursor = len(m.rows) - 1
	if height := m.listHeight(); m.cursor >= m.offset+height {
		m.offset = m.cursor - height + 1
	}
	return m, tick()
}

// mergeNotices adds warnings not already showing.
//
// Repeating the same unreadable-file warning on every poll would fill the
// footer with one message and hide everything else.
func mergeNotices(existing, incoming []string) []string {
	for _, note := range incoming {
		seen := false
		for _, have := range existing {
			if have == note {
				seen = true
				break
			}
		}
		if !seen {
			existing = append(existing, note)
		}
	}
	return existing
}
