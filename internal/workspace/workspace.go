// Package workspace remembers which log locations you are watching.
//
// A subscription is nothing more than a remembered path. There is no daemon, no
// polling, and no write to any log file — ARCHITECTURE.md's non-goals rule all
// of that out. Subscribing means "show me this directory next time too", and
// unsubscribing means "stop", with a record of both.
//
// The audit trail is append-only and separate from the current state, because a
// list you can edit is not a record of what happened.
package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	stateFile = "subscriptions.json"
	auditFile = "audit.log"
)

// Subscription is one watched location.
type Subscription struct {
	// Path is absolute and cleaned, so the same directory reached two ways is
	// one subscription.
	Path string `json:"path"`
	// Label is the display name, defaulting to the directory's base name.
	Label string `json:"label,omitempty"`
	// Active is false for an unsubscribed location that is being kept for its
	// history and its cached ingest.
	Active  bool      `json:"active"`
	AddedAt time.Time `json:"added_at"`
	// RemovedAt is when it was last unsubscribed.
	RemovedAt *time.Time `json:"removed_at,omitempty"`
}

// Name is what to show for a subscription.
func (s Subscription) Name() string {
	if s.Label != "" {
		return s.Label
	}
	return filepath.Base(s.Path)
}

// Event is one entry in the audit trail.
type Event struct {
	At     time.Time `json:"at"`
	Action string    `json:"action"`
	Path   string    `json:"path"`
	User   string    `json:"user,omitempty"`
	// Note carries the reason a subscription failed, so the trail records
	// attempts as well as successes.
	Note string `json:"note,omitempty"`
}

// Workspace is the set of remembered locations.
type Workspace struct {
	dir           string
	Subscriptions []Subscription `json:"subscriptions"`
}

// Dir resolves where the workspace lives.
//
// The config directory, not the cache: a subscription list is a user's choice
// and should survive `loupe cache clear`.
func Dir(override string) (string, error) {
	if override != "" {
		return override, nil
	}

	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config directory: %w", err)
	}
	return filepath.Join(base, "loupe"), nil
}

// Load reads the workspace, returning an empty one when none exists yet.
func Load(override string) (*Workspace, error) {
	dir, err := Dir(override)
	if err != nil {
		return nil, err
	}

	w := &Workspace{dir: dir}

	body, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		if os.IsNotExist(err) {
			return w, nil
		}
		return nil, fmt.Errorf("read subscriptions: %w", err)
	}

	if err := json.Unmarshal(body, w); err != nil {
		// A corrupt list must not stop the tool running. Say so and carry on
		// with none, rather than refusing to start.
		return &Workspace{dir: dir}, fmt.Errorf("subscriptions file is unreadable, "+
			"continuing with none: %w", err)
	}

	w.dir = dir
	return w, nil
}

// Active returns the subscriptions currently being read, oldest first.
func (w *Workspace) Active() []Subscription {
	var out []Subscription
	for _, s := range w.Subscriptions {
		if s.Active {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AddedAt.Before(out[j].AddedAt) })
	return out
}

// ActivePaths returns the paths currently being read.
func (w *Workspace) ActivePaths() []string {
	active := w.Active()
	out := make([]string, len(active))
	for i, s := range active {
		out[i] = s.Path
	}
	return out
}

// All returns every subscription, active or not, most recently touched first.
func (w *Workspace) All() []Subscription {
	out := append([]Subscription(nil), w.Subscriptions...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Active != out[j].Active {
			return out[i].Active
		}
		return out[i].AddedAt.After(out[j].AddedAt)
	})
	return out
}

// Find returns the subscription for a path.
func (w *Workspace) Find(path string) (Subscription, bool) {
	clean, err := Canonical(path)
	if err != nil {
		return Subscription{}, false
	}
	for _, s := range w.Subscriptions {
		if s.Path == clean {
			return s, true
		}
	}
	return Subscription{}, false
}

// Subscribe adds or reactivates a location.
//
// The path must exist and be a directory or a readable file; subscribing to
// something unreadable would produce a list entry that silently never works.
func (w *Workspace) Subscribe(path, label string) (Subscription, error) {
	clean, err := Canonical(path)
	if err != nil {
		w.record(Event{Action: "subscribe-failed", Path: path, Note: err.Error()})
		return Subscription{}, err
	}

	if _, err := os.Stat(clean); err != nil {
		note := fmt.Sprintf("cannot read %s: %v", clean, err)
		w.record(Event{Action: "subscribe-failed", Path: clean, Note: note})
		return Subscription{}, fmt.Errorf("%s", note)
	}

	now := time.Now().UTC()

	for i, s := range w.Subscriptions {
		if s.Path != clean {
			continue
		}
		// Re-subscribing keeps the original AddedAt, so the trail shows how
		// long this location has been known rather than resetting it.
		w.Subscriptions[i].Active = true
		w.Subscriptions[i].RemovedAt = nil
		if label != "" {
			w.Subscriptions[i].Label = label
		}
		w.record(Event{At: now, Action: "resubscribe", Path: clean})
		return w.Subscriptions[i], w.save()
	}

	sub := Subscription{Path: clean, Label: label, Active: true, AddedAt: now}
	w.Subscriptions = append(w.Subscriptions, sub)
	w.record(Event{At: now, Action: "subscribe", Path: clean})

	return sub, w.save()
}

// Unsubscribe stops reading a location without forgetting it.
//
// The entry stays, marked inactive, and its cached ingest is left alone until
// the retention window expires or the cache is cleared. Re-subscribing during
// an incident is then instant rather than a re-read.
func (w *Workspace) Unsubscribe(path string) error {
	clean, err := Canonical(path)
	if err != nil {
		return err
	}

	now := time.Now().UTC()

	for i, s := range w.Subscriptions {
		if s.Path != clean {
			continue
		}
		if !s.Active {
			return fmt.Errorf("%s is already unsubscribed", clean)
		}

		w.Subscriptions[i].Active = false
		w.Subscriptions[i].RemovedAt = &now
		w.record(Event{At: now, Action: "unsubscribe", Path: clean})
		return w.save()
	}

	return fmt.Errorf("not subscribed to %s", clean)
}

// Forget removes a subscription and its history from the current list.
//
// The audit trail keeps the record: the list is state, the trail is what
// happened, and deleting from one must not rewrite the other.
func (w *Workspace) Forget(path string) error {
	clean, err := Canonical(path)
	if err != nil {
		return err
	}

	for i, s := range w.Subscriptions {
		if s.Path != clean {
			continue
		}
		w.Subscriptions = append(w.Subscriptions[:i], w.Subscriptions[i+1:]...)
		w.record(Event{At: time.Now().UTC(), Action: "forget", Path: clean})
		return w.save()
	}

	return fmt.Errorf("not subscribed to %s", clean)
}

// Audit reads the trail, newest last.
func (w *Workspace) Audit(limit int) ([]Event, error) {
	body, err := os.ReadFile(filepath.Join(w.dir, auditFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read audit trail: %w", err)
	}

	var events []Event
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			// One unreadable line must not hide the rest of the history.
			continue
		}
		events = append(events, e)
	}

	if limit > 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	return events, nil
}

// AuditPath is where the trail is written, for reporting.
func (w *Workspace) AuditPath() string { return filepath.Join(w.dir, auditFile) }

// record appends to the audit trail.
//
// Append-only and best-effort: failing to write history must not stop the
// action the user asked for, but it must not silently rewrite history either,
// which is why nothing here ever edits an existing line.
func (w *Workspace) record(e Event) {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	if e.User == "" {
		if u, err := user.Current(); err == nil {
			e.User = u.Username
		}
	}

	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return
	}

	f, err := os.OpenFile(w.AuditPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	if body, err := json.Marshal(e); err == nil {
		// The audit trail is a convenience, not a record anything depends on.
		// Every other failure in this function is swallowed for the same
		// reason: subscribing must not fail because the log could not be
		// appended to.
		_, _ = f.Write(append(body, '\n'))
	}
}

func (w *Workspace) save() error {
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", w.dir, err)
	}

	body, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return fmt.Errorf("encode subscriptions: %w", err)
	}

	// Write and rename, so an interrupted save cannot leave a truncated list
	// that loses every subscription.
	target := filepath.Join(w.dir, stateFile)
	temp := target + ".partial"

	if err := os.WriteFile(temp, append(body, '\n'), 0o644); err != nil {
		return fmt.Errorf("write subscriptions: %w", err)
	}
	if err := os.Rename(temp, target); err != nil {
		os.Remove(temp)
		return fmt.Errorf("install subscriptions: %w", err)
	}
	return nil
}

// Canonical resolves a path to a comparable absolute form.
//
// Symlinks are resolved so that two routes to the same directory are one
// subscription rather than two that ingest the same files twice.
func Canonical(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("empty path")
	}

	expanded, err := ExpandHome(path)
	if err != nil {
		return "", err
	}

	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}

	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return filepath.Clean(abs), nil
}

// ExpandHome turns a leading ~ into the home directory.
func ExpandHome(path string) (string, error) {
	if !strings.HasPrefix(path, "~") {
		return path, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve ~: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}
