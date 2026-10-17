// Package model holds the types shared by providers, the hub, the web UI and
// the device link.
package model

import (
	"strings"
	"time"
	"unicode/utf8"
)

type Provider string

const (
	ProviderClaude Provider = "claude"
	ProviderCodex  Provider = "codex"
)

// State is the user-facing task state of a session.
type State string

const (
	StateApproval State = "approval" // waiting for a permission decision
	StateInput    State = "input"    // waiting for some other user input
	StateError    State = "error"
	StateRunning  State = "running"
	StateDone     State = "done" // turn finished recently
	StateIdle     State = "idle" // open but nothing to show
)

// Priority orders states for display; lower is more urgent.
func (s State) Priority() int {
	switch s {
	case StateApproval:
		return 0
	case StateInput:
		return 1
	case StateError:
		return 2
	case StateRunning:
		return 3
	case StateDone:
		return 4
	default:
		return 5
	}
}

// Label is the Chinese label shown in the UI and on the device.
func (s State) Label() string {
	switch s {
	case StateApproval:
		return "需授权"
	case StateInput:
		return "等待输入"
	case StateError:
		return "出错"
	case StateRunning:
		return "进行中"
	case StateDone:
		return "已完成"
	default:
		return "空闲"
	}
}

// Session is one Claude Code or Codex conversation as tracked by the hub.
type Session struct {
	ID        string    `json:"id"` // provider:session_id
	Provider  Provider  `json:"provider"`
	SessionID string    `json:"session_id"`
	Project   string    `json:"project"`
	Cwd       string    `json:"cwd"`
	Title     string    `json:"title,omitempty"`   // session name (generated, or set with /rename)
	Message   string    `json:"message,omitempty"` // first line of the assistant's latest text this turn
	Detail    string    `json:"detail,omitempty"`  // what it is doing right now
	State     State     `json:"state"`
	Since     time.Time `json:"since"`   // when State was entered
	Updated   time.Time `json:"updated"` // last observation of any kind
	Source    string    `json:"source"`  // source of the last state change
	// Agents are the session's subagents, most urgent first.
	Agents []Agent `json:"agents,omitempty"`
	// Tools tallies the tool calls finished in the current turn.
	Tools []ToolCount `json:"tools,omitempty"`
}

// Agent is a subagent working for a session (Claude Code's Agent tool, a
// Codex spawned thread).
type Agent struct {
	ID      string    `json:"id"`
	Type    string    `json:"type"`             // Explore, general-purpose, ...
	Desc    string    `json:"desc,omitempty"`   // what it was asked to do
	Detail  string    `json:"detail,omitempty"` // what it is doing now
	State   State     `json:"state"`
	Since   time.Time `json:"since"` // started
	Updated time.Time `json:"updated"`
}

type ToolCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Active reports whether the state still needs work or attention.
func (s State) Active() bool {
	return s == StateApproval || s == StateInput || s == StateError || s == StateRunning
}

// Effective returns the most urgent state among the session and its
// agents, and the agent it comes from (nil for the session itself). A
// session waiting for its next prompt while a subagent asks for approval
// is, for the user, waiting for approval.
func (s Session) Effective() (State, *Agent) {
	st, from := s.State, (*Agent)(nil)
	for i := range s.Agents {
		a := &s.Agents[i]
		if a.State.Active() && a.State.Priority() < st.Priority() {
			st, from = a.State, a
		}
	}
	return st, from
}

// Observation is a single signal about a session from one source.
type Observation struct {
	Provider  Provider
	SessionID string
	Cwd       string
	State     State // empty: no state change, only metadata
	Title     string
	Detail    string
	// Message replaces the session's message when HasMessage is set, even
	// with "" (a new turn clears the previous reply).
	Message    string
	HasMessage bool
	At         time.Time
	Source     string // "hook", "sessions", "rollout", ...
	End        bool   // the session is gone (with AgentID: that agent is)

	// With AgentID set, State, Detail and End apply to that subagent of
	// the session, not the session itself.
	AgentID   string
	AgentType string
	AgentDesc string
	// AgentLaunch says the session started a subagent whose id is not
	// known yet; its AgentType and AgentDesc wait for the agent to appear.
	AgentLaunch bool

	ToolDone string // a tool call of the current turn finished
	NewTurn  bool   // a new turn starts: reset the tool tally and message
}

// Key returns the hub key for the observed session.
func (o Observation) Key() string { return SessionKey(o.Provider, o.SessionID) }

func SessionKey(p Provider, sessionID string) string { return string(p) + ":" + sessionID }

// Window is one plan rate-limit window.
type Window struct {
	UsedPct       float64   `json:"pct"`
	ResetsAt      time.Time `json:"resets_at"`
	WindowMinutes int       `json:"window_minutes,omitempty"`
}

// Tokens is a token tally over some period.
type Tokens struct {
	Input       int64   `json:"input"`
	Output      int64   `json:"output"`
	CacheRead   int64   `json:"cache_read"`
	CacheCreate int64   `json:"cache_create"`
	CostUSD     float64 `json:"cost_usd"`
	HasCost     bool    `json:"has_cost"`
}

func (t Tokens) Total() int64 { return t.Input + t.Output + t.CacheRead + t.CacheCreate }

// Fresh counts what was new to the model: uncached input, cache writes and
// output. Cache reads re-send the same context every turn and make up ~99%
// of a Claude Code day, so they are left out of the numbers users see.
func (t Tokens) Fresh() int64 { return t.Input + t.Output + t.CacheCreate }

func (t *Tokens) Add(o Tokens) {
	t.Input += o.Input
	t.Output += o.Output
	t.CacheRead += o.CacheRead
	t.CacheCreate += o.CacheCreate
	t.CostUSD += o.CostUSD
	t.HasCost = t.HasCost || o.HasCost
}

// Usage is everything known about one provider's consumption.
type Usage struct {
	FiveHour      *Window   `json:"five_hour,omitempty"`
	SevenDay      *Window   `json:"seven_day,omitempty"`
	LimitsUpdated time.Time `json:"limits_updated"`
	LimitsSource  string    `json:"limits_source,omitempty"`
	Today         Tokens    `json:"today"`
	Week          Tokens    `json:"week"` // last 7 days including today
}

// Snapshot is the complete hub state at one point in time.
type Snapshot struct {
	Now      time.Time          `json:"now"`
	Sessions []Session          `json:"sessions"` // sorted by priority, includes idle
	Usage    map[Provider]Usage `json:"usage"`
	Counts   map[State]int      `json:"counts"`
}

// Truncate shortens s to at most n runes, adding an ellipsis when cut.
func Truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
