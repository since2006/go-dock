package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/procutil"
)

// sessionFile is ~/.claude/sessions/<pid>.json, written by every running
// Claude Code process. The format is undocumented, so every field is
// optional.
type sessionFile struct {
	PID             int    `json:"pid"`
	SessionID       string `json:"sessionId"`
	Cwd             string `json:"cwd"`
	Status          string `json:"status"` // busy, idle, waiting
	WaitingFor      string `json:"waitingFor"`
	StartedAt       int64  `json:"startedAt"`       // unix ms
	StatusUpdatedAt int64  `json:"statusUpdatedAt"` // unix ms
	UpdatedAt       int64  `json:"updatedAt"`       // unix ms
}

type trackedSession struct {
	sessionID string
	status    string
	waiting   string
	at        int64
}

type sessionsWatcher struct {
	dir   string
	alive func(pid int) bool
	known map[string]trackedSession // by file path
}

func newSessionsWatcher(dir string) *sessionsWatcher {
	return &sessionsWatcher{dir: dir, alive: procutil.Alive, known: map[string]trackedSession{}}
}

// poll reads every session file and returns observations for sessions
// whose status changed, appeared or disappeared.
func (w *sessionsWatcher) poll(now time.Time) []model.Observation {
	paths, _ := filepath.Glob(filepath.Join(w.dir, "*.json"))
	seen := make(map[string]bool, len(paths))
	var out []model.Observation
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var f sessionFile
		if json.Unmarshal(b, &f) != nil || f.SessionID == "" {
			continue // possibly caught mid-write; retry next poll
		}
		if f.PID > 0 && !w.alive(f.PID) {
			continue
		}
		seen[path] = true
		at := f.StatusUpdatedAt
		if at == 0 {
			at = f.UpdatedAt
		}
		prev, known := w.known[path]
		if known && prev.sessionID == f.SessionID && prev.status == f.Status && prev.waiting == f.WaitingFor && prev.at == at {
			continue
		}
		if known && prev.sessionID != f.SessionID {
			// The process switched sessions (/clear or /resume).
			out = append(out, model.Observation{Provider: P, SessionID: prev.sessionID, End: true, Source: "sessions"})
		}
		w.known[path] = trackedSession{sessionID: f.SessionID, status: f.Status, waiting: f.WaitingFor, at: at}

		st, detail := mapStatus(f, known && prev.sessionID == f.SessionID && prev.status != "idle")
		if st == "" {
			continue
		}
		ts := now
		if at > 0 {
			ts = time.UnixMilli(at)
		}
		out = append(out, model.Observation{
			Provider: P, SessionID: f.SessionID, Cwd: f.Cwd,
			State: st, Detail: detail, At: ts, Source: "sessions",
		})
	}
	for path, t := range w.known {
		if !seen[path] {
			delete(w.known, path)
			out = append(out, model.Observation{Provider: P, SessionID: t.sessionID, End: true, Source: "sessions"})
		}
	}
	return out
}

// mapStatus turns a status file into a state. wasActive tells whether we
// saw this session busy or waiting before, which turns idle into done.
func mapStatus(f sessionFile, wasActive bool) (model.State, string) {
	switch f.Status {
	case "busy":
		return model.StateRunning, ""
	case "waiting":
		w := strings.ToLower(f.WaitingFor)
		if w == "" || strings.Contains(w, "permission") || strings.Contains(w, "sandbox") {
			// No detail: the hook knows which tool is asking.
			return model.StateApproval, ""
		}
		return model.StateInput, waitingLabel(w)
	case "idle":
		// A session that went idle well after it started has finished a
		// turn; one that is idle right after start never ran.
		if wasActive || (f.StatusUpdatedAt-f.StartedAt > 5000 && f.StartedAt > 0) {
			return model.StateDone, ""
		}
		return model.StateIdle, ""
	}
	return "", ""
}

func waitingLabel(w string) string {
	switch w {
	case "input needed":
		return "需要输入"
	case "dialog open":
		return "对话框待处理"
	case "worker request":
		return "子任务请求"
	case "goal proposal":
		return "目标待确认"
	}
	return w
}
