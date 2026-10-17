package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/providers"
)

// agentRecent is how long a subagent transcript may stay quiet and still
// count as running when no hook says otherwise.
const agentRecent = 30 * time.Minute

// agentTrack follows one subagent transcript,
// projects/<cwd>/<session>/subagents/agent-<id>.jsonl, and its .meta.json.
// It lets go-dock list subagents even without hooks installed.
type agentTrack struct {
	session, id string
	metaPath    string

	typ, desc string
	finished  bool
	detail    string    // last tool call
	at        time.Time // last activity
	touched   bool      // new lines since the last report

	sent struct {
		state  model.State
		detail string
		desc   string
	}
}

// agentFor returns the track for a subagent transcript path, or nil.
func (t *transcripts) agentFor(path string) *agentTrack {
	if a, ok := t.agents[path]; ok {
		return a
	}
	base := filepath.Base(path)
	dir := filepath.Dir(path)
	if filepath.Base(dir) != "subagents" || !strings.HasPrefix(base, "agent-") || !strings.HasSuffix(base, ".jsonl") {
		return nil
	}
	a := &agentTrack{
		session:  filepath.Base(filepath.Dir(dir)),
		id:       strings.TrimSuffix(strings.TrimPrefix(base, "agent-"), ".jsonl"),
		metaPath: strings.TrimSuffix(path, ".jsonl") + ".meta.json",
		touched:  true,
	}
	// An agent that has not made a call yet is active since its file was
	// last written.
	if fi, err := os.Stat(path); err == nil {
		a.at = fi.ModTime()
	}
	t.agents[path] = a
	return a
}

// toolUse records the last tool call of an assistant line.
func (a *agentTrack) toolUse(at time.Time, content json.RawMessage) {
	var blocks []struct {
		Type  string          `json:"type"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if b.Type == "tool_use" {
			a.detail = providers.ToolAction(b.Name, b.Input)
		}
	}
	if at.After(a.at) {
		a.at = at
	}
	a.touched = true
}

func (a *agentTrack) readMeta() {
	b, err := os.ReadFile(a.metaPath)
	if err != nil {
		return
	}
	var m struct {
		AgentType   string `json:"agentType"`
		Description string `json:"description"`
		Finished    bool   `json:"finished"`
	}
	if json.Unmarshal(b, &m) != nil {
		return
	}
	a.typ = m.AgentType
	a.desc = model.Truncate(m.Description, providers.DetailLen)
	if m.Finished && !a.finished {
		a.finished = true
		if fi, err := os.Stat(a.metaPath); err == nil && fi.ModTime().After(a.at) {
			a.at = fi.ModTime()
		}
	}
}

// agentObservations reports subagents that changed. With all set it also
// rereads every recent agent's meta file, which flips when it finishes.
func (t *transcripts) agentObservations(now time.Time, all bool) []model.Observation {
	var out []model.Observation
	for path, a := range t.agents {
		if now.Sub(a.at) > agentRecent {
			if now.Sub(a.at) > window {
				delete(t.agents, path)
			}
			continue
		}
		if !a.touched && !all {
			continue
		}
		// Wait until the session is known to be running; old transcripts
		// must not bring sessions back.
		if t.live != nil && !t.live(a.session) {
			continue
		}
		a.touched = false
		a.readMeta()

		st, detail := model.StateRunning, a.detail
		if a.finished {
			st, detail = model.StateDone, ""
		}
		if st == a.sent.state && detail == a.sent.detail && a.desc == a.sent.desc {
			continue
		}
		a.sent.state, a.sent.detail, a.sent.desc = st, detail, a.desc
		out = append(out, model.Observation{
			Provider: P, SessionID: a.session, At: a.at, Source: "transcript",
			AgentID: a.id, AgentType: a.typ, AgentDesc: a.desc, State: st, Detail: detail,
		})
	}
	return out
}
