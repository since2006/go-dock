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
	T        string         `json:"t"`
	Seq      uint64         `json:"seq"`
	Now      int64          `json:"now"`
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

type SessionFrame struct {
	ID      string `json:"id"`
	P       string `json:"p"`
	Project string `json:"project"`
	State   string `json:"state"`
	Label   string `json:"label"`
	Text    string `json:"text"`
	Since   int64  `json:"since"`
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
		if ss.State == model.StateIdle && !o.ShowIdle {
			continue
		}
		f.Total++
		if len(f.Sessions) >= limit {
			continue
		}
		text := ss.Detail
		if text == "" {
			text = ss.Title
		}
		f.Sessions = append(f.Sessions, SessionFrame{
			ID:      ss.ID,
			P:       string(ss.Provider),
			Project: model.Truncate(ss.Project, projectLen),
			State:   string(ss.State),
			Label:   ss.State.Label(),
			Text:    model.Truncate(text, textLen),
			Since:   unix(ss.Since),
		})
	}
	return f
}

func usageFrame(u model.Usage) UsageFrame {
	f := UsageFrame{
		FiveHour:    windowFrame(u.FiveHour),
		SevenDay:    windowFrame(u.SevenDay),
		TodayTokens: u.Today.Total(),
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

// EncodeSnapshot encodes f as one line of at most MaxLine bytes, dropping
// sessions from the tail (the least urgent) until it fits.
func EncodeSnapshot(f Snapshot) ([]byte, error) {
	for {
		line, err := encodeLine(f)
		if err != nil {
			return nil, err
		}
		if len(line) <= MaxLine {
			return line, nil
		}
		if len(f.Sessions) == 0 {
			return nil, fmt.Errorf("snapshot 超过 %d 字节", MaxLine)
		}
		f.Sessions = f.Sessions[:len(f.Sessions)-1]
	}
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
