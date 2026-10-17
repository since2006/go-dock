// Package hub merges observations from every provider into one ordered view
// of sessions and usage, and tells subscribers when it changes.
package hub

import (
	"context"
	"path/filepath"
	"reflect"
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

// agentLinger keeps a finished subagent listed for a short while.
const agentLinger = 3 * time.Minute

// pendingTTL drops subagent launches no agent ever claimed.
const pendingTTL = 10 * time.Minute

type Hub struct {
	now func() time.Time

	mu       sync.Mutex
	opts     Options
	sessions map[string]*entry
	usage    map[model.Provider]model.Usage
	subs     map[chan struct{}]struct{}
}

// entry is one session plus what the hub tracks about it. Session.Agents
// and Session.Tools are only filled in by Snapshot.
type entry struct {
	s       model.Session
	stateAt time.Time // last observation of the session's own state
	agents  map[string]*model.Agent
	pending []launch
	tools   []model.ToolCount

	// The current turn's clock, paused while the session or one of its
	// agents waits for the user.
	turnStart, turnEnd time.Time
	paused             time.Duration // finished waits this turn
	pauseStart         time.Time     // set while waiting
}

// launch is a subagent the session started whose id is not known yet.
type launch struct {
	typ, desc string
	at        time.Time
}

func (e *entry) clone() entry {
	c := *e
	c.agents = make(map[string]*model.Agent, len(e.agents))
	for k, a := range e.agents {
		cp := *a
		c.agents[k] = &cp
	}
	c.pending = append([]launch(nil), e.pending...)
	c.tools = append([]model.ToolCount(nil), e.tools...)
	return c
}

func New(opts Options) *Hub {
	return &Hub{
		now:      time.Now,
		opts:     opts,
		sessions: map[string]*entry{},
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

// Observe applies one observation. Observations older than the last state
// change do not change state again, so a slow poller cannot undo a fresher
// hook event. An observation without State only updates metadata of a
// known session or agent and does not count as activity.
func (h *Hub) Observe(o model.Observation) {
	if o.SessionID == "" {
		return
	}
	if o.At.IsZero() {
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
	e, ok := h.sessions[key]
	if o.End && o.AgentID == "" {
		if ok {
			delete(h.sessions, key)
		}
		return ok
	}
	if !ok {
		if o.State == "" || (o.AgentID != "" && !o.State.Active()) {
			return false // metadata, or the end of an agent we never saw
		}
		e = &entry{
			s:      model.Session{ID: key, Provider: o.Provider, SessionID: o.SessionID, State: model.StateIdle, Since: o.At},
			agents: map[string]*model.Agent{},
		}
		h.sessions[key] = e
	}
	before := e.clone()
	s := &e.s
	// A session belongs to the directory it started in. Claude Code hooks
	// report the shell's current directory, which follows every cd.
	if o.Cwd != "" && s.Cwd == "" {
		s.Cwd = o.Cwd
		s.Project = projectName(o.Cwd)
	}
	if o.AgentLaunch {
		e.pending = append(e.pending, launch{typ: o.AgentType, desc: o.AgentDesc, at: o.At})
	}
	if o.NewTurn {
		e.tools = nil
		s.Message, s.TurnOutput = "", 0
	}
	effBefore := e.effective()
	if o.ToolDone != "" {
		e.countTool(o.ToolDone)
	}

	if o.AgentID != "" {
		h.observeAgent(e, o)
	} else {
		if o.Title != "" {
			s.Title = o.Title
		}
		if o.Turn != nil {
			s.Message, s.TurnOutput = o.Turn.Message, o.Turn.Output
		}
		if o.State != "" && !o.At.Before(e.stateAt) {
			if o.State != s.State {
				s.State = o.State
				s.Since = o.At
				s.Source = o.Source
				s.Detail = o.Detail
			} else if o.Detail != "" {
				s.Detail = o.Detail
			}
			e.stateAt = o.At
		}
	}
	e.clock(effBefore, e.effective(), o.NewTurn, o.At)
	// Any state news, the session's or an agent's, shows the session lives.
	if (o.State != "" || o.End) && o.At.After(s.Updated) {
		s.Updated = o.At
	}
	if s.State == model.StateDone && h.now().Sub(s.Since) > h.opts.DoneTTL {
		s.State = model.StateIdle
		s.Detail = ""
	}
	return !ok || !reflect.DeepEqual(before, *e)
}

func (h *Hub) observeAgent(e *entry, o model.Observation) {
	a := e.agents[o.AgentID]
	if a == nil {
		if !o.State.Active() {
			return // e.g. Claude's internal agents only report their stop
		}
		a = &model.Agent{ID: o.AgentID, Type: o.AgentType, State: o.State, Detail: o.Detail, Since: o.At, Updated: o.At}
		e.agents[o.AgentID] = a
		if o.AgentDesc == "" {
			a.Desc = e.claim(o.AgentType)
		}
	}
	if o.AgentType != "" {
		a.Type = o.AgentType
	}
	if o.AgentDesc != "" {
		a.Desc = o.AgentDesc
		e.drop(o.AgentDesc)
	}
	switch {
	case o.End:
		if a.State.Active() {
			a.State, a.Detail = model.StateDone, ""
		}
	case o.State != "" && !o.At.Before(a.Updated):
		if o.State != a.State {
			a.State, a.Detail = o.State, o.Detail
		} else if o.Detail != "" {
			a.Detail = o.Detail
		}
		if !a.State.Active() {
			a.Detail = ""
		}
	}
	if (o.State != "" || o.End) && o.At.After(a.Updated) {
		a.Updated = o.At
	}
}

// claim takes the oldest pending launch of an agent type for a new agent.
func (e *entry) claim(typ string) string {
	for i, l := range e.pending {
		if l.typ == typ || l.typ == "" || typ == "" {
			e.pending = append(e.pending[:i], e.pending[i+1:]...)
			return l.desc
		}
	}
	return ""
}

// drop forgets a pending launch once its agent is known by id.
func (e *entry) drop(desc string) {
	for i, l := range e.pending {
		if l.desc == desc {
			e.pending = append(e.pending[:i], e.pending[i+1:]...)
			return
		}
	}
}

// effective is Session.Effective for an entry, whose Session.Agents is only
// filled in by Snapshot.
func (e *entry) effective() model.State {
	st := e.s.State
	for _, a := range e.agents {
		if a.State.Active() && a.State.Priority() < st.Priority() {
			st = a.State
		}
	}
	return st
}

// clock moves the turn clock for a change of the effective state. A turn
// starts with a prompt or when a finished session gets busy, and ends when
// it is finished again; waits for approval or input do not count.
func (e *entry) clock(before, after model.State, newTurn bool, at time.Time) {
	busy := func(st model.State) bool {
		return st == model.StateRunning || st == model.StateApproval || st == model.StateInput
	}
	waiting := after == model.StateApproval || after == model.StateInput
	if newTurn || (!busy(before) && busy(after)) {
		e.turnStart, e.turnEnd, e.paused, e.pauseStart = at, time.Time{}, 0, time.Time{}
	}
	if e.turnStart.IsZero() {
		return
	}
	if !e.pauseStart.IsZero() && !waiting {
		if at.After(e.pauseStart) {
			e.paused += at.Sub(e.pauseStart)
		}
		e.pauseStart = time.Time{}
	}
	if waiting && e.pauseStart.IsZero() {
		e.pauseStart = at
	}
	if busy(before) && !busy(after) {
		e.turnEnd = at
	}
}

// turnSecs is the turn's work time at now.
func (e *entry) turnSecs(now time.Time) int64 {
	if e.turnStart.IsZero() {
		return 0
	}
	end := now
	if !e.turnEnd.IsZero() {
		end = e.turnEnd
	}
	if !e.pauseStart.IsZero() && e.pauseStart.Before(end) {
		end = e.pauseStart
	}
	return int64(max(end.Sub(e.turnStart)-e.paused, 0) / time.Second)
}

func (e *entry) countTool(name string) {
	for i := range e.tools {
		if e.tools[i].Name == name {
			e.tools[i].Count++
			return
		}
	}
	e.tools = append(e.tools, model.ToolCount{Name: name, Count: 1})
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
	for _, e := range h.sessions {
		if e.s.Provider == p {
			ids = append(ids, e.s.SessionID)
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

// Tick ages done sessions to idle and drops stale sessions, finished
// agents and unclaimed launches.
func (h *Hub) Tick() {
	now := h.now()
	changed := false
	h.mu.Lock()
	for k, e := range h.sessions {
		s := &e.s
		if now.Sub(s.Updated) > h.opts.StaleAfter {
			delete(h.sessions, k)
			changed = true
			continue
		}
		if s.State == model.StateDone && now.Sub(s.Since) > h.opts.DoneTTL {
			s.State = model.StateIdle
			s.Detail = ""
			changed = true
		}
		for id, a := range e.agents {
			if (!a.State.Active() && now.Sub(a.Updated) > agentLinger) || now.Sub(a.Updated) > h.opts.StaleAfter {
				delete(e.agents, id)
				changed = true
			}
		}
		kept := e.pending[:0]
		for _, l := range e.pending {
			if now.Sub(l.at) <= pendingTTL {
				kept = append(kept, l)
			}
		}
		e.pending = kept
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
// urgency (their own or an agent's), then most recently updated first.
// Counts are by that effective state.
func (h *Hub) Snapshot() model.Snapshot {
	now := h.now()
	h.mu.Lock()
	snap := model.Snapshot{
		Now:      now,
		Sessions: make([]model.Session, 0, len(h.sessions)),
		Usage:    make(map[model.Provider]model.Usage, len(h.usage)),
		Counts:   map[model.State]int{},
	}
	for _, e := range h.sessions {
		s := e.s
		s.Tools = append([]model.ToolCount(nil), e.tools...)
		s.TurnSecs = e.turnSecs(now)
		for _, a := range e.agents {
			s.Agents = append(s.Agents, *a)
		}
		sort.Slice(s.Agents, func(i, j int) bool {
			a, b := s.Agents[i], s.Agents[j]
			if pa, pb := a.State.Priority(), b.State.Priority(); pa != pb {
				return pa < pb
			}
			if !a.Since.Equal(b.Since) {
				return a.Since.Before(b.Since)
			}
			return a.ID < b.ID
		})
		snap.Sessions = append(snap.Sessions, s)
		st, _ := s.Effective()
		snap.Counts[st]++
	}
	for p, u := range h.usage {
		u.FiveHour = expire(u.FiveHour, now)
		u.SevenDay = expire(u.SevenDay, now)
		snap.Usage[p] = u
	}
	h.mu.Unlock()

	sort.Slice(snap.Sessions, func(i, j int) bool {
		a, b := snap.Sessions[i], snap.Sessions[j]
		sa, _ := a.Effective()
		sb, _ := b.Effective()
		if pa, pb := sa.Priority(), sb.Priority(); pa != pb {
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
	for k, e := range h.sessions {
		if e.s.Provider == p {
			delete(h.sessions, k)
		}
	}
	delete(h.usage, p)
	h.mu.Unlock()
	h.notify()
}

// Touch tells subscribers to refresh, for state kept outside the hub.
func (h *Hub) Touch() { h.notify() }
