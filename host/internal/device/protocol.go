// Package device talks to the ESP32-S3 display over USB serial using the
// newline-delimited JSON protocol in docs/protocol-v1.md.
package device

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
)

const (
	// ProtoVersion is the protocol major version spoken by this host.
	ProtoVersion = 1
	// MaxLine bounds one message, including its trailing newline.
	MaxLine = 4096

	projectLen = 24
	textLen    = 48
	titleLen   = 32
	msgLen     = 48
	// maxAgents bounds the subagents listed per session.
	maxAgents    = 3
	agentTypeLen = 16
	agentTextLen = 32
)

// Hello is the host's handshake message.
type Hello struct {
	T     string `json:"t"`
	Proto int    `json:"proto"`
	Host  string `json:"host"`
	Ver   string `json:"ver"`
}

// Snapshot is the full state pushed to the device.
type Snapshot struct {
	T   string `json:"t"`
	Seq uint64 `json:"seq"`
	Now int64  `json:"now"`
	// TZ is the host's UTC offset in seconds, so the device clock shows
	// local time.
	TZ       int            `json:"tz"`
	Usage    UsageFrames    `json:"usage"`
	Sessions []SessionFrame `json:"sessions"`
	Counts   Counts         `json:"counts"`
	// Total is the number of displayable sessions before the MaxSessions
	// cap and the line-size cap, so the device can show "+N".
	Total int `json:"total"`
}

type UsageFrames struct {
	Claude UsageFrame `json:"claude"`
	Codex  UsageFrame `json:"codex"`
}

type UsageFrame struct {
	FiveHour    *WindowFrame `json:"five_hour"`
	SevenDay    *WindowFrame `json:"seven_day"`
	TodayTokens int64        `json:"today_tokens"`
	TodayCost   *float64     `json:"today_cost,omitempty"`
	UpdatedAt   int64        `json:"updated_at"`
}

type WindowFrame struct {
	Pct       float64 `json:"pct"`
	ResetsAt  int64   `json:"resets_at"`
	WindowMin int     `json:"window_min"`
}

// SessionFrame is one task row. State, Label, Text and Since describe the
// most urgent of the session and its subagents, so a subagent waiting for
// approval puts the whole row in "approval".
type SessionFrame struct {
	ID      string `json:"id"`
	P       string `json:"p"`
	Project string `json:"project"`
	State   string `json:"state"`
	Label   string `json:"label"`
	// Text is one line for simple screens: the current action, else the
	// title; when a subagent decides the row's state, "Type · action".
	Text  string `json:"text"`
	Since int64  `json:"since"`
	// Title names the session; Msg is the first line of the assistant's
	// latest text this turn and Detail what the session itself is doing
	// now, so a screen can show all three.
	Title  string `json:"title,omitempty"`
	Msg    string `json:"msg,omitempty"`
	Detail string `json:"detail,omitempty"`
	// TurnOutput counts the output tokens of the session's current turn;
	// TurnSecs is its work time at Now, without waits for the user. A
	// screen keeps counting from it while the row is running.
	TurnOutput int64 `json:"turn_output,omitempty"`
	TurnSecs   int64 `json:"turn_secs,omitempty"`
	// Agents lists up to three subagents, most urgent first. AgentsTotal
	// and AgentsActive count all of them; all three are omitted when the
	// session has none.
	Agents       []AgentFrame `json:"agents,omitempty"`
	AgentsTotal  int          `json:"agents_total,omitempty"`
	AgentsActive int          `json:"agents_active,omitempty"`
}

type AgentFrame struct {
	Type  string `json:"type"`  // Explore, general-purpose, ...
	Text  string `json:"text"`  // its task; what it waits on when it needs the user
	State string `json:"state"` // running, approval, input, error, done
	Since int64  `json:"since"`
}

type Counts struct {
	Approval int `json:"approval"`
	Input    int `json:"input"`
	Running  int `json:"running"`
	Done     int `json:"done"`
	Error    int `json:"error"`
	Idle     int `json:"idle"`
}

// BuildFrame converts a hub snapshot into the snapshot message. Sessions
// arrive sorted by urgency; idle ones are dropped unless ShowIdle, and at
// most MaxSessions are kept.
func BuildFrame(s model.Snapshot, o Options, seq uint64) Snapshot {
	f := Snapshot{
		T:   "snapshot",
		Seq: seq,
		Now: s.Now.Unix(),
		TZ:  tzOffset(s.Now),
		Usage: UsageFrames{
			Claude: usageFrame(s.Usage[model.ProviderClaude]),
			Codex:  usageFrame(s.Usage[model.ProviderCodex]),
		},
		Sessions: []SessionFrame{},
		Counts: Counts{
			Approval: s.Counts[model.StateApproval],
			Input:    s.Counts[model.StateInput],
			Running:  s.Counts[model.StateRunning],
			Done:     s.Counts[model.StateDone],
			Error:    s.Counts[model.StateError],
			Idle:     s.Counts[model.StateIdle],
		},
	}
	limit := o.MaxSessions
	if limit <= 0 {
		limit = 12
	}
	for _, ss := range s.Sessions {
		st, from := ss.Effective()
		if st == model.StateIdle && !o.ShowIdle {
			continue
		}
		f.Total++
		if len(f.Sessions) >= limit {
			continue
		}
		f.Sessions = append(f.Sessions, sessionFrame(ss, st, from))
	}
	return f
}

func sessionFrame(ss model.Session, st model.State, from *model.Agent) SessionFrame {
	text, since := ss.Detail, ss.Since
	if text == "" {
		text = ss.Title
	}
	if from != nil {
		// The row's state comes from a subagent: say which one and why.
		what := from.Desc
		if from.State != model.StateRunning || what == "" {
			what = from.Detail
		}
		text, since = from.Type, from.Since
		if what != "" {
			text += " · " + what
		}
	}
	sf := SessionFrame{
		ID:      ss.ID,
		P:       string(ss.Provider),
		Project: model.Truncate(ss.Project, projectLen),
		State:   string(st),
		Label:   st.Label(),
		Text:    model.Truncate(text, textLen),
		Since:   unix(since),
		Title:   model.Truncate(ss.Title, titleLen),
		Msg:     model.Truncate(ss.Message, msgLen),
		Detail:  model.Truncate(ss.Detail, textLen),

		TurnOutput: ss.TurnOutput,
		TurnSecs:   ss.TurnSecs,
	}
	if from != nil && (ss.State == model.StateDone || ss.State == model.StateIdle) {
		// The session's own turn ended while background subagents work.
		active := 0
		for _, a := range ss.Agents {
			if a.State.Active() {
				active++
			}
		}
		sf.Detail = fmt.Sprintf("等待 %d 个子任务完成", active)
	}
	if sf.Title == sf.Detail {
		sf.Title = "" // nothing new on a second line
	}
	for _, a := range ss.Agents {
		sf.AgentsTotal++
		if a.State.Active() {
			sf.AgentsActive++
		}
		if len(sf.Agents) < maxAgents {
			// Its task, unless it is stuck on the user: then what it asks.
			t := a.Desc
			if t == "" || (a.State != model.StateRunning && a.State != model.StateDone && a.Detail != "") {
				t = a.Detail
			}
			sf.Agents = append(sf.Agents, AgentFrame{
				Type:  model.Truncate(a.Type, agentTypeLen),
				Text:  model.Truncate(t, agentTextLen),
				State: string(a.State),
				Since: unix(a.Since),
			})
		}
	}
	return sf
}

func usageFrame(u model.Usage) UsageFrame {
	f := UsageFrame{
		FiveHour:    windowFrame(u.FiveHour),
		SevenDay:    windowFrame(u.SevenDay),
		TodayTokens: u.Today.Fresh(),
		UpdatedAt:   unix(u.LimitsUpdated),
	}
	if u.Today.HasCost {
		c := u.Today.CostUSD
		f.TodayCost = &c
	}
	return f
}

func windowFrame(w *model.Window) *WindowFrame {
	if w == nil {
		return nil
	}
	return &WindowFrame{Pct: w.UsedPct, ResetsAt: unix(w.ResetsAt), WindowMin: w.WindowMinutes}
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// encodeLine marshals v as one line ending in '\n', without HTML escaping.
func encodeLine(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// EncodeSnapshot encodes f as one line of at most MaxLine bytes. When it
// is too long it first drops listed subagents, starting with the least
// urgent session (counts stay), then messages the same way, then sessions
// from the tail.
func EncodeSnapshot(f Snapshot) ([]byte, error) {
	f.Sessions = append([]SessionFrame(nil), f.Sessions...) // trimmed below
	for {
		line, err := encodeLine(f)
		if err != nil {
			return nil, err
		}
		if len(line) <= MaxLine {
			return line, nil
		}
		if dropAgent(f.Sessions) || dropMsg(f.Sessions) {
			continue
		}
		if len(f.Sessions) == 0 {
			return nil, fmt.Errorf("snapshot 超过 %d 字节", MaxLine)
		}
		f.Sessions = f.Sessions[:len(f.Sessions)-1]
	}
}

// dropAgent removes the last listed subagent of the last session that has
// one, and reports whether there was any.
func dropAgent(ss []SessionFrame) bool {
	for i := len(ss) - 1; i >= 0; i-- {
		if n := len(ss[i].Agents); n > 0 {
			ss[i].Agents = ss[i].Agents[: n-1 : n-1]
			return true
		}
	}
	return false
}

// dropMsg clears the message of the last session that has one, and reports
// whether there was any.
func dropMsg(ss []SessionFrame) bool {
	for i := len(ss) - 1; i >= 0; i-- {
		if ss[i].Msg != "" {
			ss[i].Msg = ""
			return true
		}
	}
	return false
}

// EncodeHello encodes the host handshake.
func EncodeHello(hostVersion string) []byte {
	line, _ := encodeLine(Hello{T: "hello", Proto: ProtoVersion, Host: "go-dock", Ver: hostVersion})
	return line
}

// DeviceMsg is any message the device sends; T selects which fields apply.
type DeviceMsg struct {
	T string `json:"t"`
	// hello
	Proto   int    `json:"proto"`
	FW      string `json:"fw"`
	W       int    `json:"w"`
	H       int    `json:"h"`
	MaxLine int    `json:"max_line"`
	// ack
	Seq uint64 `json:"seq"`
	// log
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

var errNotMessage = errors.New("不是协议消息")

// DecodeDeviceLine parses one line from the device. Lines that are not a
// JSON object with a "t" field (e.g. boot logs) return errNotMessage.
func DecodeDeviceLine(line []byte) (DeviceMsg, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return DeviceMsg{}, errNotMessage
	}
	var m DeviceMsg
	if err := json.Unmarshal(line, &m); err != nil || m.T == "" {
		return DeviceMsg{}, errNotMessage
	}
	return m, nil
}

func tzOffset(t time.Time) int {
	_, off := t.Zone()
	return off
}
