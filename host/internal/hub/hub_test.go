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
