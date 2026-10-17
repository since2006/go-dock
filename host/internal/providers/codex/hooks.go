package codex

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/providers"
)

// HandleHook maps one Codex hook payload to the hub.
func (p *Provider) HandleHook(body []byte) error {
	o, ok, err := hookObservation(body, time.Now())
	if err != nil {
		return err
	}
	if ok {
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
	if h.AgentID != "" {
		// A spawned subagent thread: session_id is the root session and
		// agent_id the subagent's thread.
		o.AgentID, o.AgentType = h.AgentID, h.AgentType
		switch h.Event {
		case "SubagentStart":
			o.State = model.StateRunning
		case "UserPromptSubmit":
			o.State = model.StateRunning
			o.AgentDesc = model.Truncate(h.Prompt, providers.DetailLen)
		case "PreToolUse", "PostToolUse":
			o.State = model.StateRunning
			o.Detail = providers.ToolSummary(h.ToolName, h.ToolInput)
		case "PermissionRequest":
			o.State = model.StateApproval
			o.Detail = providers.ToolSummary(h.ToolName, h.ToolInput)
		case "SubagentStop", "Stop", "Interrupt":
			o.State = model.StateDone
		default:
			return o, false, nil
		}
		return o, true, nil
	}
	switch h.Event {
	case "SessionStart":
		o.State = model.StateIdle
	case "UserPromptSubmit":
		o.State = model.StateRunning
		o.Title = model.Truncate(h.Prompt, providers.TitleLen)
		o.NewTurn = true
	case "PreToolUse":
		o.State = model.StateRunning
		o.Detail = providers.ToolSummary(h.ToolName, h.ToolInput)
	case "PostToolUse":
		o.State = model.StateRunning
		o.Detail = providers.ToolSummary(h.ToolName, h.ToolInput)
		o.ToolDone = h.ToolName
	case "PermissionRequest":
		o.State = model.StateApproval
		o.Detail = providers.ToolSummary(h.ToolName, h.ToolInput)
	case "Stop":
		o.State = model.StateDone
	case "Interrupt":
		o.State = model.StateIdle
		o.Detail = "已中断"
	case "SessionEnd":
		o.End = true
	default:
		return o, false, nil
	}
	return o, true, nil
}
