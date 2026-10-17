// Package hub merges observations from every provider into one ordered view
// of sessions and usage, and tells subscribers when it changes.
package hub

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
)

type Options struct {
	DoneTTL    time.Duration // done -> idle after this long
	StaleAfter time.Duration // drop sessions with no news for this long
}

type Hub struct {
	now func() time.Time

	mu       sync.Mutex
	opts     Options
	sessions map[string]*model.Session
	usage    map[model.Provider]model.Usage
	subs     map[chan struct{}]struct{}
}

func New(opts Options) *Hub {
	return &Hub{
		now:      time.Now,
		opts:     opts,
		sessions: map[string]*model.Session{},
		usage:    map[model.Provider]model.Usage{},
		subs:     map[chan struct{}]struct{}{},
	}
}

func (h *Hub) SetOptions(o Options) {
	h.mu.Lock()
	h.opts = o
	h.mu.Unlock()
	h.Tick()
}

// Observe applies one observation. Observations older than the session's
// last update do not change its state, so a slow poller cannot undo a
// fresher hook event. An observation without State only updates metadata
// of a known session and does not count as activity.
func (h *Hub) Observe(o model.Observation) {
	if o.SessionID == "" {
		return
	}
	if o.At.IsZero() && (o.State != "" || o.End) {
		o.At = h.now()
	}
	h.mu.Lock()
	changed := h.observe(o)
	h.mu.Unlock()
	if changed {
		h.notify()
	}
}

func (h *Hub) observe(o model.Observation) bool {
	key := o.Key()
	s, ok := h.sessions[key]
	if o.End {
		if ok {
			delete(h.sessions, key)
		}
		return ok
	}
	if !ok {
		if o.State == "" {
			return false // metadata alone does not create a session
		}
		s = &model.Session{ID: key, Provider: o.Provider, SessionID: o.SessionID, State: model.StateIdle, Since: o.At}
		h.sessions[key] = s
	}
	before := *s
	if o.Cwd != "" {
		s.Cwd = o.Cwd
		s.Project = projectName(o.Cwd)
	}
	if o.Title != "" {
		s.Title = o.Title
	}
	if o.State != "" && !o.At.Before(s.Updated) {
		if o.State != s.State {
			s.State = o.State
			s.Since = o.At
			s.Source = o.Source
			s.Detail = o.Detail
		} else if o.Detail != "" {
			s.Detail = o.Detail
		}
	}
	if o.At.After(s.Updated) {
		s.Updated = o.At
	}
	if s.State == model.StateDone && h.now().Sub(s.Since) > h.opts.DoneTTL {
		s.State = model.StateIdle
		s.Detail = ""
	}
	return !ok || *s != before
}

func projectName(cwd string) string {
	cwd = strings.TrimRight(strings.ReplaceAll(cwd, `\`, "/"), "/")
	if cwd == "" {
		return ""
	}
	return filepath.Base(filepath.FromSlash(cwd))
}

// Sessions returns the IDs of tracked sessions for one provider.
func (h *Hub) Sessions(p model.Provider) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var ids []string
	for _, s := range h.sessions {
		if s.Provider == p {
			ids = append(ids, s.SessionID)
		}
	}
	return ids
}

// UpdateUsage edits one provider's usage in place.
func (h *Hub) UpdateUsage(p model.Provider, f func(u *model.Usage)) {
	h.mu.Lock()
	u := h.usage[p]
	before := u
	f(&u)
	h.usage[p] = u
	changed := !usageEqual(before, u)
	h.mu.Unlock()
	if changed {
		h.notify()
	}
}

func usageEqual(a, b model.Usage) bool {
	wEq := func(x, y *model.Window) bool {
		if x == nil || y == nil {
			return x == y
		}
		return *x == *y
	}
	return wEq(a.FiveHour, b.FiveHour) && wEq(a.SevenDay, b.SevenDay) &&
		a.LimitsUpdated.Equal(b.LimitsUpdated) && a.LimitsSource == b.LimitsSource &&
		a.Today == b.Today && a.Week == b.Week
}

// Tick ages done sessions to idle and drops stale ones.
func (h *Hub) Tick() {
	now := h.now()
	changed := false
	h.mu.Lock()
	for k, s := range h.sessions {
		switch {
		case now.Sub(s.Updated) > h.opts.StaleAfter:
			delete(h.sessions, k)
			changed = true
		case s.State == model.StateDone && now.Sub(s.Since) > h.opts.DoneTTL:
			s.State = model.StateIdle
			s.Detail = ""
			changed = true
		}
	}
	h.mu.Unlock()
	if changed {
		h.notify()
	}
}

// Run calls Tick periodically until ctx ends.
func (h *Hub) Run(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.Tick()
		}
	}
}

// Snapshot returns a copy of the current state with sessions ordered by
// urgency, then most recently updated first.
func (h *Hub) Snapshot() model.Snapshot {
	now := h.now()
	h.mu.Lock()
	snap := model.Snapshot{
		Now:      now,
		Sessions: make([]model.Session, 0, len(h.sessions)),
		Usage:    make(map[model.Provider]model.Usage, len(h.usage)),
		Counts:   map[model.State]int{},
	}
	for _, s := range h.sessions {
		snap.Sessions = append(snap.Sessions, *s)
		snap.Counts[s.State]++
	}
	for p, u := range h.usage {
		u.FiveHour = expire(u.FiveHour, now)
		u.SevenDay = expire(u.SevenDay, now)
		snap.Usage[p] = u
	}
	h.mu.Unlock()

	sort.Slice(snap.Sessions, func(i, j int) bool {
		a, b := snap.Sessions[i], snap.Sessions[j]
		if pa, pb := a.State.Priority(), b.State.Priority(); pa != pb {
			return pa < pb
		}
		if !a.Updated.Equal(b.Updated) {
			return a.Updated.After(b.Updated)
		}
		return a.ID < b.ID
	})
	return snap
}

// expire reports a window whose reset time has passed as empty, since the
// provider has not told us the new value yet.
func expire(w *model.Window, now time.Time) *model.Window {
	if w == nil {
		return nil
	}
	c := *w
	if !c.ResetsAt.IsZero() && !c.ResetsAt.After(now) {
		c.UsedPct = 0
		c.ResetsAt = time.Time{}
	}
	return &c
}

// Subscribe returns a channel that receives a value after changes. Bursts
// are coalesced, so readers should fetch a fresh Snapshot on each receive.
func (h *Hub) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

func (h *Hub) notify() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// RemoveProvider forgets every session and the usage of p, e.g. after the
// user disabled that provider.
func (h *Hub) RemoveProvider(p model.Provider) {
	h.mu.Lock()
	for k, s := range h.sessions {
		if s.Provider == p {
			delete(h.sessions, k)
		}
	}
	delete(h.usage, p)
	h.mu.Unlock()
	h.notify()
}
