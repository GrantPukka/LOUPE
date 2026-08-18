package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/GrantPukka/loupe/internal/session"
	"github.com/GrantPukka/loupe/internal/store"
)

// tailBuffer is how many events a slow client may fall behind by.
//
// Deep enough that a browser busy laying out a burst of rows does not lose
// any, shallow enough that a tab left open on a paused laptop cannot pin an
// unbounded amount of memory. Overflowing it is reported, never silent.
const tailBuffer = 32

// tailHeartbeat keeps an idle stream from being closed by the client.
//
// Also how a disconnect is noticed on a quiet directory: nothing is written
// during a silence, so without this a closed connection would not surface
// until the next record arrived, and the poll loop would keep running for a
// client that had gone.
const tailHeartbeat = 20 * time.Second

// tailHub is the single follower behind every live stream.
//
// One per server, not one per connection. A Follower carries its own view of
// where it has read to in each file, and Poll rewinds to the start of the last
// record and re-reads it. Two of them over the same store would each rewind
// past the other's writes, so two open tabs would see duplicated lines in one
// and missing lines in the other. Sharing one follower also keeps the store's
// writes on a single goroutine, which is what makes them safe alongside the
// query handlers.
//
// The poll loop runs only while somebody is subscribed. There is no daemon
// here: an idle `loupe serve` polls nothing and touches no files.
type tailHub struct {
	sess *session.Session

	mu      sync.Mutex
	subs    map[*tailSub]struct{}
	stop    context.CancelFunc
	running bool
}

// tailSub is one connected client: its filter, and somewhere to put records.
type tailSub struct {
	plan   session.Plan
	events chan tailEvent

	// dropped counts events the buffer could not hold. A client that has
	// fallen behind is told so rather than quietly shown an incomplete tail.
	dropped int
}

// tailEvent is one SSE message.
type tailEvent struct {
	name string
	data any
}

// tailRecords is the payload of a records event.
//
// The same shape as a query response's rows, so the client renders live and
// queried records with one code path and they cannot drift apart.
type tailRecords struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
	// Total is the running record count of the whole dataset, so the footer
	// and the histogram know something changed underneath them.
	Total    int64  `json:"total"`
	Timezone string `json:"timezone"`
}

// tailNotice carries a per-source read failure to the client.
//
// A file that became unreadable mid-incident is exactly the kind of thing that
// must not be swallowed: the remaining sources still stream, and the user is
// told which one stopped.
type tailNotice struct {
	Message string `json:"message"`
}

// tailLag reports that a client fell behind and records were dropped from its
// stream. It is not a substitute for the records, it is an instruction to
// re-run the query, because the store still holds them.
type tailLag struct {
	Dropped int    `json:"dropped"`
	Message string `json:"message"`
}

func newTailHub(sess *session.Session) *tailHub {
	return &tailHub{sess: sess, subs: map[*tailSub]struct{}{}}
}

// subscribe registers a client and starts the poll loop if it is the first.
func (h *tailHub) subscribe(sub *tailSub) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.subs[sub] = struct{}{}
	if h.running {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	h.stop, h.running = cancel, true
	go h.loop(ctx)
}

// unsubscribe removes a client and stops polling once the last one leaves.
func (h *tailHub) unsubscribe(sub *tailSub) {
	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.subs, sub)
	if len(h.subs) > 0 || !h.running {
		return
	}
	h.stop()
	h.running = false
}

// loop polls until the last subscriber goes away.
func (h *tailHub) loop(ctx context.Context) {
	follower, err := h.sess.Follower(ctx)
	if err != nil {
		h.broadcast(tailEvent{name: "notice", data: tailNotice{
			Message: fmt.Sprintf("could not start following: %v", err),
		}})
		return
	}

	ticker := time.NewTicker(store.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.pollOnce(ctx, follower)
		}
	}
}

// pollOnce reads what arrived and hands each subscriber its own view of it.
//
// The per-subscriber queries run here, on this goroutine, rather than being
// handed the batch to query for themselves. Predicate() excludes a boundary
// record by file and line number, and the next poll may delete and reinsert
// that record under a new sequence number — so a query run after the next poll
// would select the wrong rows. Doing the work before returning is what keeps
// the stream and a later query in agreement.
func (h *tailHub) pollOnce(ctx context.Context, follower *store.Follower) {
	batch, err := follower.Poll(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		h.broadcast(tailEvent{name: "notice", data: tailNotice{Message: err.Error()}})
		return
	}

	for _, e := range batch.Errors {
		h.broadcast(tailEvent{name: "notice", data: tailNotice{Message: e.Error()}})
	}
	if batch.Records == 0 {
		return
	}

	where, args := batch.Predicate()
	total, err := h.sess.DB.Count(ctx)
	if err != nil {
		total = 0
	}

	for _, sub := range h.subscribers() {
		res, err := h.sess.Records(ctx, sub.plan, session.RecordQuery{
			Sort:      session.SortTime,
			Columns:   tailColumns,
			Where:     where,
			WhereArgs: args,
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			h.send(sub, tailEvent{name: "notice", data: tailNotice{Message: err.Error()}})
			continue
		}
		if res.RowCount() == 0 {
			// Records arrived but none matched this client's filter. Not
			// something to report on every tick.
			continue
		}

		h.send(sub, tailEvent{name: "records", data: tailRecords{
			Columns:  res.Columns,
			Rows:     res.Rows,
			Total:    total,
			Timezone: h.sess.Loc.String(),
		}})
	}
}

// tailColumns is the selection live rows arrive with.
//
// Identical to what the UI's record list asks for, so a streamed row and a
// queried row are the same shape and the client has one renderer, not two.
const tailColumns = "seq, ts, level, source, message"

func (h *tailHub) subscribers() []*tailSub {
	h.mu.Lock()
	defer h.mu.Unlock()

	out := make([]*tailSub, 0, len(h.subs))
	for sub := range h.subs {
		out = append(out, sub)
	}
	return out
}

func (h *tailHub) broadcast(ev tailEvent) {
	for _, sub := range h.subscribers() {
		h.send(sub, ev)
	}
}

// send queues an event, or counts it as dropped if the client is behind.
//
// Blocking here would stall the poll loop, and with it every other client, on
// one browser tab that has stopped reading. Dropping is the lesser evil, and
// it is declared: the next successful send carries a lag event saying how many
// were lost and that the records are still in the store.
func (h *tailHub) send(sub *tailSub, ev tailEvent) {
	select {
	case sub.events <- ev:
	default:
		h.mu.Lock()
		sub.dropped++
		h.mu.Unlock()
	}
}

// marshalEvent encodes an event payload for an SSE data line.
//
// JSON never contains a literal newline, so one data line always holds the
// whole payload and the client does not have to reassemble it.
func marshalEvent(data any) ([]byte, error) {
	body, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("encode live event: %w", err)
	}
	return body, nil
}

func (s *Server) handleTail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// The filter is resolved before the stream opens, so a typo comes back as
	// the CLI's error with its spelling suggestion rather than as a stream
	// that connects and then silently shows nothing.
	plan, err := s.sess.Plan(ctx, r.URL.Query().Get("filter"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError,
			fmt.Errorf("this server cannot stream responses"))
		return
	}

	// The server sets a write deadline so a slow query cannot hold a
	// connection forever. A live tail is the one response that is meant to
	// stay open, so it clears its own.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		writeError(w, http.StatusInternalServerError,
			fmt.Errorf("could not open a live stream: %w", err))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Buffering proxies break SSE. There is no proxy on loopback, but this
	// costs nothing and makes an SSH-tunnelled setup behave.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sub := &tailSub{plan: plan, events: make(chan tailEvent, tailBuffer)}
	s.tail.subscribe(sub)
	defer s.tail.unsubscribe(sub)

	heartbeat := time.NewTicker(tailHeartbeat)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case ev := <-sub.events:
			if n := s.tail.claimDropped(sub); n > 0 {
				if err := writeEvent(w, flusher, tailEvent{name: "lag", data: tailLag{
					Dropped: n,
					Message: fmt.Sprintf("the live stream fell behind and dropped %d update(s). "+
						"The records are in the store — re-run the filter to see them.", n),
				}}); err != nil {
					return
				}
			}
			if err := writeEvent(w, flusher, ev); err != nil {
				return
			}

		case <-heartbeat.C:
			// A comment line: valid SSE, ignored by EventSource, and the write
			// is what fails once the client has gone.
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// claimDropped reads and clears a subscriber's dropped count under the lock
// that the poll loop increments it under.
func (h *tailHub) claimDropped(sub *tailSub) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	n := sub.dropped
	sub.dropped = 0
	return n
}

// writeEvent sends one SSE message.
func writeEvent(w http.ResponseWriter, flusher http.Flusher, ev tailEvent) error {
	body, err := marshalEvent(ev.data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, body); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}
