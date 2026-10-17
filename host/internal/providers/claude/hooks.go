package claude

import (
	"encoding/json"
	"fmt"
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
			p.mu.Lock()
			if o.Title == "" {
				o.Title = p.titles[o.SessionID]
			}
			p.mu.Unlock()
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
	SessionID  string `json:"session_id"`
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
	if s.RateLimits == nil {
		return nil // API-key users and fresh sessions have none
	}
	five, seven := s.RateLimits.FiveHour.window(300), s.RateLimits.SevenDay.window(10080)
	if five == nil && seven == nil {
		return nil
	}
	now := time.Now()
	p.sink.UpdateUsage(P, func(u *model.Usage) {
		// Claude Code drops a window once it resets. Keep the old one; the
		// hub reports a window past its reset time as 0%.
		if five != nil {
			u.FiveHour = five
		}
		if seven != nil {
			u.SevenDay = seven
		}
		u.LimitsUpdated = now
		u.LimitsSource = "statusline"
	})
	return nil
}
