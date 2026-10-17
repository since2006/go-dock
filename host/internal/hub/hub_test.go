package hub

import (
	"testing"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
)

var t0 = time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)

func newTestHub(now *time.Time) *Hub {
	h := New(Options{DoneTTL: 30 * time.Minute, StaleAfter: 12 * time.Hour})
	h.now = func() time.Time { return *now }
	return h
}

func obs(id string, st model.State, at time.Time, detail string) model.Observation {
	return model.Observation{Provider: model.ProviderClaude, SessionID: id, State: st, At: at, Detail: detail, Source: "test"}
}

func TestStateTransitions(t *testing.T) {
	now := t0
	h := newTestHub(&now)

	h.Observe(model.Observation{Provider: model.ProviderClaude, SessionID: "a", Cwd: "/work/go-dock", State: model.StateRunning, At: t0, Title: "fix tests"})
	h.Observe(obs("a", model.StateApproval, t0.Add(time.Second), "Bash: npm install"))
	s := h.Snapshot().Sessions[0]
	if s.State != model.StateApproval || s.Detail != "Bash: npm install" || s.Project != "go-dock" || s.Title != "fix tests" {
		t.Fatalf("unexpected session: %+v", s)
	}

	// An older observation must not override the newer state.
	h.Observe(obs("a", model.StateRunning, t0.Add(500*time.Millisecond), ""))
	if got := h.Snapshot().Sessions[0].State; got != model.StateApproval {
		t.Fatalf("stale observation applied: %s", got)
	}

	// Same state without detail keeps the previous detail.
	h.Observe(obs("a", model.StateApproval, t0.Add(2*time.Second), ""))
	if got := h.Snapshot().Sessions[0].Detail; got != "Bash: npm install" {
		t.Fatalf("detail lost: %q", got)
	}

	h.Observe(obs("a", model.StateDone, t0.Add(3*time.Second), ""))
	s = h.Snapshot().Sessions[0]
	if s.State != model.StateDone || s.Detail != "" || !s.Since.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("unexpected done session: %+v", s)
	}

	h.Observe(model.Observation{Provider: model.ProviderClaude, SessionID: "a", End: true})
	if n := len(h.Snapshot().Sessions); n != 0 {
		t.Fatalf("session not removed, have %d", n)
	}
}

func TestSessionKeepsStartDirectory(t *testing.T) {
	now := t0
	h := newTestHub(&now)
	h.Observe(model.Observation{Provider: model.ProviderClaude, SessionID: "a", Cwd: "/work/go-dock", State: model.StateRunning, At: t0})
	// Claude Code hooks report the directory the shell has cd'ed into.
	h.Observe(model.Observation{Provider: model.ProviderClaude, SessionID: "a", Cwd: "/work/go-dock/host", State: model.StateRunning, At: t0.Add(time.Second)})
	if s := h.Snapshot().Sessions[0]; s.Project != "go-dock" || s.Cwd != "/work/go-dock" {
		t.Fatalf("project followed cd: %+v", s)
	}
}

func TestOrdering(t *testing.T) {
	now := t0
	h := newTestHub(&now)
	h.Observe(obs("done", model.StateDone, t0.Add(4*time.Second), ""))
	h.Observe(obs("run-old", model.StateRunning, t0.Add(1*time.Second), ""))
	h.Observe(obs("run-new", model.StateRunning, t0.Add(2*time.Second), ""))
	h.Observe(obs("idle", model.StateIdle, t0.Add(5*time.Second), ""))
	h.Observe(obs("perm", model.StateApproval, t0, ""))

	var got []string
	for _, s := range h.Snapshot().Sessions {
		got = append(got, s.SessionID)
	}
	want := []string{"perm", "run-new", "run-old", "done", "idle"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestTickAgesAndDrops(t *testing.T) {
	now := t0
	h := newTestHub(&now)
	h.Observe(obs("done", model.StateDone, t0, "x"))
	h.Observe(obs("old", model.StateRunning, t0, ""))

	now = t0.Add(31 * time.Minute)
	h.Observe(obs("old", model.StateRunning, now, "")) // keep it fresh
	h.Tick()
	snap := h.Snapshot()
	if snap.Counts[model.StateIdle] != 1 || snap.Counts[model.StateRunning] != 1 {
		t.Fatalf("counts = %v", snap.Counts)
	}

	now = now.Add(13 * time.Hour)
	h.Tick()
	if n := len(h.Snapshot().Sessions); n != 0 {
		t.Fatalf("stale sessions kept: %d", n)
	}
}

func TestUsageExpiry(t *testing.T) {
	now := t0
	h := newTestHub(&now)
	ch, cancel := h.Subscribe()
	defer cancel()

	h.UpdateUsage(model.ProviderCodex, func(u *model.Usage) {
		u.FiveHour = &model.Window{UsedPct: 40, ResetsAt: t0.Add(time.Hour)}
	})
	select {
	case <-ch:
	default:
		t.Fatal("no change notification")
	}
	if got := h.Snapshot().Usage[model.ProviderCodex].FiveHour.UsedPct; got != 40 {
		t.Fatalf("pct = %v", got)
	}
	now = t0.Add(2 * time.Hour)
	if w := h.Snapshot().Usage[model.ProviderCodex].FiveHour; w.UsedPct != 0 || !w.ResetsAt.IsZero() {
		t.Fatalf("expired window not cleared: %+v", w)
	}

	// Identical updates do not notify.
	h.UpdateUsage(model.ProviderCodex, func(u *model.Usage) {})
	select {
	case <-ch:
		t.Fatal("unexpected notification")
	default:
	}
}

func TestMetadataOnly(t *testing.T) {
	now := t0
	h := newTestHub(&now)
	h.Observe(model.Observation{Provider: model.ProviderClaude, SessionID: "x", Title: "t"})
	if n := len(h.Snapshot().Sessions); n != 0 {
		t.Fatalf("metadata created a session")
	}
	h.Observe(obs("x", model.StateRunning, t0, ""))
	now = t0.Add(time.Hour)
	h.Observe(model.Observation{Provider: model.ProviderClaude, SessionID: "x", Title: "new title"})
	s := h.Snapshot().Sessions[0]
	if s.Title != "new title" || !s.Updated.Equal(t0) {
		t.Fatalf("metadata update wrong: %+v", s)
	}

	// An old done observation lands directly as idle.
	h.Observe(obs("y", model.StateDone, t0, ""))
	for _, s := range h.Snapshot().Sessions {
		if s.SessionID == "y" && s.State != model.StateIdle {
			t.Fatalf("old done not aged: %s", s.State)
		}
	}
}

func agentObs(session, agent string, st model.State, at time.Time, detail string) model.Observation {
	return model.Observation{Provider: model.ProviderClaude, SessionID: session, AgentID: agent, AgentType: "Explore", State: st, At: at, Detail: detail, Source: "hook"}
}

func TestAgentsDoNotOverwriteSession(t *testing.T) {
	now := t0
	h := newTestHub(&now)
	h.Observe(obs("s", model.StateRunning, t0, "Agent: 查找接口"))
	h.Observe(agentObs("s", "a1", model.StateRunning, t0.Add(time.Second), "Read: main.go"))
	s := h.Snapshot().Sessions[0]
	if s.Detail != "Agent: 查找接口" || s.State != model.StateRunning {
		t.Fatalf("session overwritten by agent: %+v", s)
	}
	if len(s.Agents) != 1 || s.Agents[0].Detail != "Read: main.go" || s.Agents[0].Type != "Explore" {
		t.Fatalf("agents = %+v", s.Agents)
	}
	if !s.Updated.Equal(t0.Add(time.Second)) {
		t.Fatal("agent activity did not keep the session fresh")
	}

	// A session state change older than the agent's news still applies:
	// ordering is per session state, not per agent activity.
	h.Observe(obs("s", model.StateDone, t0.Add(500*time.Millisecond), ""))
	if got := h.Snapshot().Sessions[0].State; got != model.StateDone {
		t.Fatalf("session state = %s", got)
	}
}

func TestAgentLaunchDescriptions(t *testing.T) {
	now := t0
	h := newTestHub(&now)
	launch := func(desc string) model.Observation {
		return model.Observation{Provider: model.ProviderClaude, SessionID: "s", State: model.StateRunning, At: t0,
			AgentLaunch: true, AgentType: "Explore", AgentDesc: desc}
	}
	h.Observe(launch("看工具栏"))
	h.Observe(launch("看对话框"))
	h.Observe(agentObs("s", "a1", model.StateRunning, t0.Add(time.Second), ""))
	h.Observe(agentObs("s", "a2", model.StateRunning, t0.Add(2*time.Second), ""))
	agents := h.Snapshot().Sessions[0].Agents
	if agents[0].Desc != "看工具栏" || agents[1].Desc != "看对话框" {
		t.Fatalf("descriptions not claimed in launch order: %+v", agents)
	}
	// The exact id-to-description mapping from the tool response wins.
	h.Observe(model.Observation{Provider: model.ProviderClaude, SessionID: "s", AgentID: "a1", AgentDesc: "看对话框"})
	h.Observe(model.Observation{Provider: model.ProviderClaude, SessionID: "s", AgentID: "a2", AgentDesc: "看工具栏"})
	agents = h.Snapshot().Sessions[0].Agents
	if agents[0].Desc != "看对话框" || agents[1].Desc != "看工具栏" {
		t.Fatalf("descriptions = %+v", agents)
	}
}

func TestAgentLifecycle(t *testing.T) {
	now := t0
	h := newTestHub(&now)

	// The stop of an agent nobody saw start (Claude's internal agents) is
	// ignored and creates nothing.
	h.Observe(agentObs("ghost", "x", model.StateDone, t0, ""))
	if n := len(h.Snapshot().Sessions); n != 0 {
		t.Fatalf("stop created a session")
	}

	h.Observe(obs("s", model.StateDone, t0, ""))
	h.Observe(agentObs("s", "a1", model.StateRunning, t0, ""))
	h.Observe(agentObs("s", "a2", model.StateApproval, t0, "Bash: rm -rf dist"))
	snap := h.Snapshot()
	s := snap.Sessions[0]
	if st, from := s.Effective(); st != model.StateApproval || from == nil || from.ID != "a2" {
		t.Fatalf("effective = %s %+v", st, from)
	}
	if snap.Counts[model.StateApproval] != 1 || snap.Counts[model.StateDone] != 0 {
		t.Fatalf("counts = %v", snap.Counts)
	}
	if s.Agents[0].ID != "a2" {
		t.Fatalf("agents not ordered by urgency: %+v", s.Agents)
	}

	now = t0.Add(time.Minute)
	h.Observe(agentObs("s", "a2", model.StateDone, now, ""))
	h.Observe(model.Observation{Provider: model.ProviderClaude, SessionID: "s", AgentID: "a1", End: true, At: now})
	for _, a := range h.Snapshot().Sessions[0].Agents {
		if a.State != model.StateDone || a.Detail != "" {
			t.Fatalf("agent not done: %+v", a)
		}
	}
	now = now.Add(agentLinger + time.Second)
	h.Tick()
	if n := len(h.Snapshot().Sessions[0].Agents); n != 0 {
		t.Fatalf("finished agents kept: %d", n)
	}

	h.Observe(agentObs("s", "a3", model.StateRunning, now, ""))
	h.Observe(model.Observation{Provider: model.ProviderClaude, SessionID: "s", End: true})
	if n := len(h.Snapshot().Sessions); n != 0 {
		t.Fatal("session end kept the session")
	}
}

func TestToolTally(t *testing.T) {
	now := t0
	h := newTestHub(&now)
	tool := func(name string) model.Observation {
		return model.Observation{Provider: model.ProviderClaude, SessionID: "s", State: model.StateRunning, ToolDone: name}
	}
	h.Observe(tool("Read"))
	h.Observe(tool("Bash"))
	h.Observe(tool("Read"))
	tools := h.Snapshot().Sessions[0].Tools
	if len(tools) != 2 || tools[0] != (model.ToolCount{Name: "Read", Count: 2}) || tools[1].Count != 1 {
		t.Fatalf("tools = %+v", tools)
	}
	h.Observe(model.Observation{Provider: model.ProviderClaude, SessionID: "s", State: model.StateRunning, NewTurn: true})
	if tools := h.Snapshot().Sessions[0].Tools; len(tools) != 0 {
		t.Fatalf("new turn kept tally: %+v", tools)
	}
}

func TestPendingLaunchesExpire(t *testing.T) {
	now := t0
	h := newTestHub(&now)
	h.Observe(model.Observation{Provider: model.ProviderClaude, SessionID: "s", State: model.StateRunning, At: t0,
		AgentLaunch: true, AgentType: "Explore", AgentDesc: "old"})
	now = t0.Add(pendingTTL + time.Second)
	h.Observe(obs("s", model.StateRunning, now, "")) // keep the session fresh
	h.Tick()
	h.Observe(agentObs("s", "a1", model.StateRunning, now, ""))
	if d := h.Snapshot().Sessions[0].Agents[0].Desc; d != "" {
		t.Fatalf("expired launch claimed: %q", d)
	}
}
