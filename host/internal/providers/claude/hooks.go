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
	o, ok, err := hookObservation(body, time.Now())
	if err != nil {
		return err
	}
	if ok {
		p.mu.Lock()
		if o.Title == "" {
			o.Title = p.titles[o.SessionID]
		}
		p.mu.Unlock()
		p.sink.Observe(o)
	}
	return nil
}

func hookObservation(body []byte, now time.Time) (model.Observation, bool, error) {
	var h providers.HookPayload
	if err := json.Unmarshal(body, &h); err != nil {
		return model.Observation{}, false, fmt.Errorf("hook payload: %w", err)
	}
	if h.SessionID == "" {
		return model.Observation{}, false, fmt.Errorf("hook payload without session_id")
	}
	o := model.Observation{Provider: P, SessionID: h.SessionID, Cwd: h.Cwd, At: now, Source: "hook"}
	switch h.Event {
	case "SessionStart":
		o.State = model.StateIdle
	case "UserPromptSubmit":
		o.State = model.StateRunning
	case "PreToolUse", "PostToolUse", "PostToolUseFailure", "PermissionDenied":
		o.State = model.StateRunning
		o.Detail = providers.ToolSummary(h.ToolName, h.ToolInput)
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
			return o, false, nil
		}
	case "Stop":
		o.State = model.StateDone
	case "StopFailure":
		o.State = model.StateError
		o.Detail = model.Truncate(h.ErrorText(), providers.DetailLen)
	case "SessionEnd":
		o.End = true
	default:
		return o, false, nil
	}
	return o, true, nil
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
