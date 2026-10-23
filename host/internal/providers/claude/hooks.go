package claude

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/providers"
)

// HandleHook maps one Claude Code hook payload to the hub.
func (p *Provider) HandleHook(body []byte) error {
	obs, err := hookObservations(body, time.Now())
	if err != nil {
		return err
	}
	for _, o := range obs {
		if o.AgentID == "" && o.State != "" {
			p.fromTranscript(&o)
		}
		p.sink.Observe(o)
	}
	return nil
}

func hookObservations(body []byte, now time.Time) ([]model.Observation, error) {
	var h providers.HookPayload
	if err := json.Unmarshal(body, &h); err != nil {
		return nil, fmt.Errorf("hook payload: %w", err)
	}
	if h.SessionID == "" {
		return nil, fmt.Errorf("hook payload without session_id")
	}
	o := model.Observation{Provider: P, SessionID: h.SessionID, Cwd: h.Cwd, At: now, Source: "hook"}
	if h.AgentID != "" {
		// Inside a subagent: the event is about that agent, not the session.
		o.AgentID, o.AgentType = h.AgentID, h.AgentType
		switch h.Event {
		case "SubagentStart":
			o.State = model.StateRunning
		case "PreToolUse", "PostToolUse", "PostToolUseFailure", "PermissionDenied":
			o.State = model.StateRunning
			o.Detail = providers.ToolAction(h.ToolName, h.ToolInput)
		case "PermissionRequest":
			o.State = model.StateApproval
			o.Detail = providers.ToolSummary(h.ToolName, h.ToolInput)
		case "Notification":
			if h.NotificationType != "permission_prompt" {
				return nil, nil
			}
			o.State = model.StateApproval
		case "SubagentStop":
			o.State = model.StateDone
		case "StopFailure":
			o.State = model.StateError
			o.Detail = model.Truncate(h.ErrorText(), providers.DetailLen)
		default:
			return nil, nil
		}
		return []model.Observation{o}, nil
	}

	switch h.Event {
	case "SessionStart":
		o.State = model.StateIdle
	case "UserPromptSubmit":
		o.State = model.StateRunning
		o.NewTurn = true
	case "PreToolUse", "PermissionDenied":
		o.State = model.StateRunning
		o.Detail = providers.ToolAction(h.ToolName, h.ToolInput)
		if h.Event == "PreToolUse" {
			// These tools stop and wait for the user to answer.
			switch h.ToolName {
			case "AskUserQuestion":
				o.State, o.Detail = model.StateInput, "等待回答问题"
			case "ExitPlanMode":
				o.State, o.Detail = model.StateInput, "等待确认计划"
			}
		}
		if h.Event == "PreToolUse" && providers.AgentTool(h.ToolName) {
			o.AgentLaunch = true
			o.AgentType, o.AgentDesc = providers.AgentLaunch(h.ToolInput)
		}
	case "PostToolUse", "PostToolUseFailure":
		o.State = model.StateRunning
		o.Detail = providers.ToolAction(h.ToolName, h.ToolInput)
		o.ToolDone = h.ToolName
		if providers.AgentTool(h.ToolName) {
			// The response names the agent the call started.
			var resp struct {
				AgentID string `json:"agentId"`
			}
			if json.Unmarshal(h.ToolResponse, &resp) == nil && resp.AgentID != "" {
				typ, desc := providers.AgentLaunch(h.ToolInput)
				return []model.Observation{o, {
					Provider: P, SessionID: h.SessionID, At: now, Source: "hook",
					AgentID: resp.AgentID, AgentType: typ, AgentDesc: desc,
				}}, nil
			}
		}
	case "PermissionRequest":
		o.State = model.StateApproval
		o.Detail = providers.ToolSummary(h.ToolName, h.ToolInput)
	case "Notification":
		switch h.NotificationType {
		case "permission_prompt":
			o.State = model.StateApproval
		case "elicitation_dialog", "agent_needs_input":
			o.State = model.StateInput
			o.Detail = model.Truncate(h.Message, providers.DetailLen)
		default:
			return nil, nil
		}
	case "Stop":
		o.State = model.StateDone
	case "StopFailure":
		o.State = model.StateError
		o.Detail = model.Truncate(h.ErrorText(), providers.DetailLen)
	case "SessionEnd":
		o.End = true
	default:
		return nil, nil
	}
	return []model.Observation{o}, nil
}

// statuslinePayload is the part of the statusLine stdin JSON go-dock uses.
type statuslinePayload struct {
	SessionID string `json:"session_id"`
	Cost      *struct {
		APIDurationMs *float64 `json:"total_api_duration_ms"` // grows with every API reply
	} `json:"cost"`
	RateLimits *struct {
		FiveHour *rateWindow `json:"five_hour"`
		SevenDay *rateWindow `json:"seven_day"`
	} `json:"rate_limits"`
}

type rateWindow struct {
	UsedPercentage *float64 `json:"used_percentage"`
	ResetsAt       *int64   `json:"resets_at"` // unix seconds
}

func (w *rateWindow) window(minutes int) *model.Window {
	if w == nil || w.UsedPercentage == nil {
		return nil
	}
	out := &model.Window{UsedPct: *w.UsedPercentage, WindowMinutes: minutes}
	if w.ResetsAt != nil && *w.ResetsAt > 0 {
		out.ResetsAt = time.Unix(*w.ResetsAt, 0)
	}
	return out
}

// HandleStatusline takes plan rate limits from a statusLine payload.
func (p *Provider) HandleStatusline(body []byte) error {
	var s statuslinePayload
	if err := json.Unmarshal(body, &s); err != nil {
		return fmt.Errorf("statusline payload: %w", err)
	}
	r := limitReport{at: time.Now()}
	if s.Cost != nil && s.Cost.APIDurationMs != nil {
		r.apiMs = *s.Cost.APIDurationMs
	}
	if s.RateLimits != nil { // API-key users and fresh sessions have none
		r.five, r.seven = s.RateLimits.FiveHour.window(300), s.RateLimits.SevenDay.window(10080)
	}

	t := &p.limits
	t.mu.Lock()
	defer t.mu.Unlock()
	freshFive, freshSeven := t.report(s.SessionID, r)
	if r.five == nil && r.seven == nil {
		return nil
	}
	p.sink.UpdateUsage(P, func(u *model.Usage) {
		f, ff := pick(u.FiveHour, r.five, freshFive, t.fiveFresh)
		w, wf := pick(u.SevenDay, r.seven, freshSeven, t.sevenFresh)
		if f == u.FiveHour && w == u.SevenDay {
			return // an idle session repeating older limits
		}
		u.FiveHour, u.SevenDay = f, w
		t.fiveFresh, t.sevenFresh = ff, wf
		u.LimitsUpdated = r.at
		u.LimitsSource = "statusline"
	})
	return nil
}

// limitForget drops what a session reported after this long without a
// report; its next one counts as a first.
const limitForget = 24 * time.Hour

// limitTracker tells news from repeats in statusline reports. Every open
// Claude Code session runs its statusline, an idle one with the limits of
// its last API reply, maybe from yesterday or from before the user reset
// their usage, so the last report is not always the newest.
type limitTracker struct {
	mu       sync.Mutex
	sessions map[string]limitReport // last report by session id
	// Whether the current windows came from a fresh report.
	fiveFresh, sevenFresh bool
}

type limitReport struct {
	apiMs       float64
	five, seven *model.Window
	at          time.Time
}

// report records a session's report and tells which of its windows are
// fresh: the session got an API reply since its previous report (its API
// time grew, or the window changed). The first report of a session may be
// old.
func (t *limitTracker) report(id string, r limitReport) (five, seven bool) {
	if id == "" {
		return false, false
	}
	if t.sessions == nil {
		t.sessions = map[string]limitReport{}
	}
	for k, old := range t.sessions {
		if r.at.Sub(old.at) > limitForget {
			delete(t.sessions, k)
		}
	}
	prev, ok := t.sessions[id]
	t.sessions[id] = r
	if !ok {
		return false, false
	}
	replied := r.apiMs > prev.apiMs
	// A window that disappears (Claude Code drops one once it resets) is
	// no reply, and leaves the other window as old as it was.
	return r.five != nil && (replied || !sameWindow(r.five, prev.five)),
		r.seven != nil && (replied || !sameWindow(r.seven, prev.seven))
}

func sameWindow(a, b *model.Window) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.UsedPct == b.UsedPct && a.ResetsAt.Equal(b.ResetsAt)
}

// pick chooses between the window we have (fresh or not) and one a
// statusline reported. A fresh report wins, lower or not: usage drops
// within a window when the user resets it. Until one arrives, e.g. after
// go-dock starts, the later window wins, and within the same one the
// higher number. An older window never does.
func pick(cur, in *model.Window, fresh, curFresh bool) (*model.Window, bool) {
	known := in != nil && cur != nil && !in.ResetsAt.IsZero() && !cur.ResetsAt.IsZero()
	switch {
	case in == nil:
		// Keep the old window; the hub reports a window past its reset
		// time as 0%.
		return cur, curFresh
	case cur == nil:
		return in, fresh
	case known && in.ResetsAt.Before(cur.ResetsAt):
		return cur, curFresh
	case fresh:
		return in, true
	case known && in.ResetsAt.After(cur.ResetsAt):
		return in, false // newer, though maybe not the newest
	case !curFresh && in.UsedPct > cur.UsedPct:
		return in, false
	}
	return cur, curFresh
}
