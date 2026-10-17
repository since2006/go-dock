// Package providers holds what the Claude Code and Codex collectors share.
package providers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/since2006/go-dock/host/internal/model"
)

// Sink receives what providers observe. *hub.Hub implements it.
type Sink interface {
	Observe(model.Observation)
	UpdateUsage(model.Provider, func(*model.Usage))
}

// DetailLen, TitleLen and MessageLen bound what the web UI shows; the
// device protocol cuts them further for the 600x450 screen.
const (
	DetailLen  = 80
	TitleLen   = 48
	MessageLen = 80
)

// MessageLine reduces the assistant's text to one display line: the first
// line with words in it, without Markdown markers.
func MessageLine(text string) string {
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "```") || strings.Trim(l, "-*_=|: ") == "" {
			continue // fences, rules, table separators
		}
		l = strings.TrimLeft(l, "#> ")
		for _, p := range []string{"- ", "* ", "+ "} {
			l = strings.TrimPrefix(l, p)
		}
		l = strings.NewReplacer("**", "", "__", "", "`", "").Replace(l)
		if l = strings.TrimSpace(l); l != "" {
			return model.Truncate(l, MessageLen)
		}
	}
	return ""
}

// HookPayload is the subset of hook stdin JSON that go-dock uses. Claude
// Code and Codex share these field names.
type HookPayload struct {
	Event            string          `json:"hook_event_name"`
	SessionID        string          `json:"session_id"`
	Cwd              string          `json:"cwd"`
	ToolName         string          `json:"tool_name"`
	ToolInput        json.RawMessage `json:"tool_input"`
	NotificationType string          `json:"notification_type"`
	Message          string          `json:"message"`
	Error            json.RawMessage `json:"error"`
	Prompt           string          `json:"prompt"`
	LastMessage      string          `json:"last_assistant_message"`
	Source           string          `json:"source"`
	Reason           string          `json:"reason"`
	// Set when the hook fires inside a subagent (and on SubagentStart/Stop).
	AgentID      string          `json:"agent_id"`
	AgentType    string          `json:"agent_type"`
	ToolResponse json.RawMessage `json:"tool_response"`
}

// AgentTool is the name of the tool that starts a subagent; older Claude
// Code versions called it Task.
func AgentTool(name string) bool { return name == "Agent" || name == "Task" }

// AgentLaunch reads the subagent type and task description from an Agent
// tool call.
func AgentLaunch(input json.RawMessage) (typ, desc string) {
	var in struct {
		Description  string `json:"description"`
		SubagentType string `json:"subagent_type"`
	}
	_ = json.Unmarshal(input, &in)
	return in.SubagentType, model.Truncate(in.Description, DetailLen)
}

// ErrorText renders the error field, which may be a string or an object.
func (p HookPayload) ErrorText() string {
	if len(p.Error) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(p.Error, &s) == nil {
		return s
	}
	var o struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if json.Unmarshal(p.Error, &o) == nil {
		if o.Message != "" {
			return o.Message
		}
		return o.Type
	}
	return ""
}

// ToolSummary renders "Tool: argument" for display, picking the most
// telling argument of common tools.
func ToolSummary(tool string, input json.RawMessage) string {
	if tool == "" {
		return ""
	}
	var m map[string]any
	_ = json.Unmarshal(input, &m)
	arg := ""
	for _, k := range []string{"command", "cmd", "file_path", "path", "notebook_path", "url", "pattern", "query", "description", "prompt"} {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch x := v.(type) {
		case string:
			arg = x
		case []any: // Codex passes argv arrays
			parts := make([]string, 0, len(x))
			for _, e := range x {
				if s, ok := e.(string); ok {
					parts = append(parts, s)
				}
			}
			if len(parts) >= 3 && (parts[1] == "-lc" || parts[1] == "-c") {
				parts = parts[2:] // drop the "bash -lc" wrapper
			}
			arg = strings.Join(parts, " ")
		}
		if arg != "" {
			if strings.HasSuffix(k, "path") {
				arg = filepath.Base(arg)
			}
			break
		}
	}
	if arg == "" {
		return model.Truncate(tool, DetailLen)
	}
	return model.Truncate(tool+": "+arg, DetailLen)
}

// ToolAction is what a running tool call is doing: the description Claude
// writes for a command ("Check hooks install"), as Claude Code shows it
// above the command, else ToolSummary. Approvals keep ToolSummary so the
// screen shows the command actually being approved.
func ToolAction(tool string, input json.RawMessage) string {
	if !AgentTool(tool) { // an agent's description is its task, shown as "Agent: …"
		var in struct {
			Description string `json:"description"`
		}
		_ = json.Unmarshal(input, &in)
		if d := strings.TrimSpace(in.Description); d != "" {
			return model.Truncate(d, DetailLen)
		}
	}
	return ToolSummary(tool, input)
}

// ResolveHome returns override, else $env, else ~/def.
func ResolveHome(override, env, def string) string {
	if override != "" {
		return override
	}
	if v := os.Getenv(env); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return def
	}
	return filepath.Join(home, def)
}
