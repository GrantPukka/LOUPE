package server

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/GrantPukka/loupe/internal/workspace"
)

// requireLocalHost rejects a request whose Host header is not loopback.
//
// This is the defence against DNS rebinding, and it exists because the browse
// endpoint below turns this process into something that reads directories on
// request. Binding to 127.0.0.1 is not enough on its own: a page on
// attacker.example can point a hostname it controls at 127.0.0.1, wait for the
// browser's DNS cache to flip, and then make same-origin requests to this
// server from a tab the user has open. The browser sends the attacker's
// hostname in Host, which is what this check catches.
func requireLocalHost(r *http.Request) error {
	host := r.Host
	if host == "" {
		return fmt.Errorf("request has no Host header")
	}

	name, _, err := net.SplitHostPort(host)
	if err != nil {
		name = host
	}

	if name == "localhost" {
		return nil
	}
	if ip := net.ParseIP(strings.Trim(name, "[]")); ip != nil && ip.IsLoopback() {
		return nil
	}

	return fmt.Errorf("refusing a request for host %q: loupe only answers to "+
		"localhost, because it reads your log files and has no authentication", host)
}

// browseResponse is one directory listing.
type browseResponse struct {
	workspace.Listing
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	if s.work == nil {
		writeError(w, http.StatusNotFound,
			fmt.Errorf("browsing is unavailable: no workspace is loaded"))
		return
	}

	listing, err := s.work.Browse(r.URL.Query().Get("path"))
	if err != nil {
		// A path outside the roots is the user's to correct, and the message
		// says how, so it comes back as a 400 rather than a bare refusal.
		writeError(w, http.StatusBadRequest, err)
		return
	}

	writeJSON(w, http.StatusOK, browseResponse{Listing: listing})
}

// subscriptionResponse is the current watch list.
type subscriptionResponse struct {
	Subscriptions []subscriptionInfo `json:"subscriptions"`
	// Reload tells the UI the ingested set has changed and the page should
	// reconnect to a restarted server.
	Reload bool `json:"reload"`
	// Note explains what the caller must do for the change to take effect.
	Note string `json:"note,omitempty"`
}

type subscriptionInfo struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Active    bool   `json:"active"`
	AddedAt   string `json:"added_at"`
	RemovedAt string `json:"removed_at,omitempty"`
	// Loaded marks a subscription that is in the running session, as opposed
	// to one added since it started.
	Loaded bool `json:"loaded"`
}

func (s *Server) handleSubscriptions(w http.ResponseWriter, r *http.Request) {
	if s.work == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("no workspace is loaded"))
		return
	}
	writeJSON(w, http.StatusOK, s.subscriptionState(""))
}

type subscribeRequest struct {
	Path  string `json:"path"`
	Label string `json:"label"`
}

func (s *Server) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.work == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("no workspace is loaded"))
		return
	}

	var req subscribeRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	if _, err := s.work.Subscribe(req.Path, req.Label); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// Ingesting a new location means rebuilding the session, which the running
	// server does not do in place. Saying so is honest; pretending the records
	// are already there would not be.
	writeJSON(w, http.StatusOK, s.subscriptionState(
		"Subscribed. Restart loupe serve to read it — the records are not in this session yet."))
}

func (s *Server) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if s.work == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("no workspace is loaded"))
		return
	}

	var req subscribeRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	if err := s.work.Unsubscribe(req.Path); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	writeJSON(w, http.StatusOK, s.subscriptionState(
		"Unsubscribed. Its cached records are kept for 14 days, so re-subscribing is instant. "+
			"Restart loupe serve to drop it from this session."))
}

// subscriptionState renders the watch list, marking which entries the running
// session actually loaded.
func (s *Server) subscriptionState(note string) subscriptionResponse {
	loaded := map[string]bool{}
	for _, path := range s.sess.Paths {
		if clean, err := workspace.Canonical(path); err == nil {
			loaded[clean] = true
		}
	}

	out := subscriptionResponse{Note: note}
	for _, sub := range s.work.All() {
		info := subscriptionInfo{
			Path:    sub.Path,
			Name:    sub.Name(),
			Active:  sub.Active,
			AddedAt: sub.AddedAt.Format("2006-01-02 15:04"),
			Loaded:  loaded[sub.Path],
		}
		if sub.RemovedAt != nil {
			info.RemovedAt = sub.RemovedAt.Format("2006-01-02 15:04")
		}

		// An active subscription the session never loaded, or a loaded one no
		// longer subscribed, both mean the screen and the list disagree.
		if info.Active != info.Loaded {
			out.Reload = true
		}
		out.Subscriptions = append(out.Subscriptions, info)
	}

	return out
}

// handleAudit returns the trail of subscribe and unsubscribe events.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if s.work == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("no workspace is loaded"))
		return
	}

	events, err := s.work.Audit(200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"events": events,
		"file":   s.work.AuditPath(),
	})
}
