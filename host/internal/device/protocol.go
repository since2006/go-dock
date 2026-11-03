// Package device talks to the ESP32-S3 display over USB serial using the
// newline-delimited JSON protocol in docs/protocol-v1.md.
package device

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/since2006/go-dock/host/internal/model"
)

const (
	// ProtoVersion is the protocol major version spoken by this host.
	ProtoVersion = 1
	// MaxLine bounds one message, including its trailing newline.
	MaxLine = 4096

	projectLen = 24
	titleLen   = 32
	// msg and detail fill a whole line under the project line: lineWidth
	// columns, wide (CJK) characters counting two, and at most lineBytes
	// bytes so the firmware's 152-byte buffers never cut a character.
	lineWidth = 64
	lineBytes = 148
	// maxAgents bounds the subagents listed per session.
	maxAgents    = 3
	agentTypeLen = 16
	agentTextLen = 32

	// The line of readings on the Agent page (§3.5).
	maxReadings     = 8
	readingLabelLen = 4
	readingTextLen  = 8 // also the sub
	readingUnitLen  = 4
)

// Hello is the host's handshake message. Name and ID tell the screen which
// computer this is; Code is the pairing code a network link must carry.
type Hello struct {
	T     string `json:"t"`
	Proto int    `json:"proto"`
	Name  string `json:"name,omitempty"`
	ID    string `json:"id,omitempty"`
	Code  string `json:"code,omitempty"`
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
	// Since is when the row entered its state: the subagent's time when one
	// decides it.
	Since int64 `json:"since"`
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
	since := ss.Since
	if from != nil {
		since = from.Since // the subagent's row says which one and why
	}
	sf := SessionFrame{
		ID:      ss.ID,
		P:       string(ss.Provider),
		Project: model.Truncate(ss.Project, projectLen),
		State:   string(st),
		Since:   unix(since),
		Title:   model.Truncate(ss.Title, titleLen),
		Msg:     fitLine(ss.Message),
		Detail:  fitLine(ss.Detail),

		TurnOutput: ss.TurnOutput,
		TurnSecs:   ss.TurnSecs,
	}
	if from != nil && (ss.State == model.StateDone || ss.State == model.StateIdle) {
		// The session's own turn ended while background subagents work.
		active := 0
		for _, a := range ss.Agents {
			if a.State.Busy() {
				active++
			}
		}
		if active > 0 {
			sf.Detail = fmt.Sprintf("等待 %d 个子任务完成", active)
		}
	}
	if sf.Title == sf.Detail {
		sf.Title = "" // nothing new on a second line
	}
	for _, a := range ss.Agents {
		sf.AgentsTotal++
		if a.State.Busy() {
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

// fitLine cuts s to its first line and to one screen line (lineWidth,
// lineBytes), ending in "…" when cut.
func fitLine(s string) string {
	s = model.Truncate(s, len(s)+1) // first line, trimmed
	w, n, cut := 0, 0, 0
	for i, r := range s {
		rw, rn := 1, utf8.RuneLen(r)
		if wide(r) {
			rw = 2
		}
		if w+rw <= lineWidth-1 && n+rn <= lineBytes-len("…") {
			cut = i + rn // the longest prefix that still has room for "…"
		}
		if w += rw; w > lineWidth {
			return s[:cut] + "…"
		}
		if n += rn; n > lineBytes {
			return s[:cut] + "…"
		}
	}
	return s
}

// wide reports characters drawn two columns wide: CJK, Hangul, full-width
// forms and emoji.
func wide(r rune) bool {
	return r >= 0x1100 && (r <= 0x115F || (r >= 0x2E80 && r <= 0xA4CF) || (r >= 0xAC00 && r <= 0xD7A3) ||
		(r >= 0xF900 && r <= 0xFAFF) || (r >= 0xFE30 && r <= 0xFE4F) || (r >= 0xFF00 && r <= 0xFF60) ||
		(r >= 0xFFE0 && r <= 0xFFE6) || r >= 0x1F300)
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

// Line carries the readings shown in a line on the Agent page, above the
// usage (docs/protocol-v1.md §3.5).
type Line struct {
	T     string         `json:"t"`
	Items []ReadingFrame `json:"items"`
}

// ReadingFrame is one reading, its texts cut to fit the screen.
type ReadingFrame struct {
	ID    string `json:"id"`
	Icon  string `json:"icon,omitempty"`
	Label string `json:"label"`
	Text  string `json:"text"`
	Unit  string `json:"unit,omitempty"`
	Sub   string `json:"sub,omitempty"`
	Pct   *int   `json:"pct,omitempty"`
	Level string `json:"level,omitempty"`
}

// protoID is what reading and icon ids may look like.
var protoID = regexp.MustCompile(`^[a-z0-9_-]{1,16}$`)

// BuildLine converts the plugins' readings into the line message: at most
// maxReadings, texts cut to fit, readings with an unusable id dropped.
func BuildLine(rs []model.Reading) Line {
	f := Line{T: "line", Items: []ReadingFrame{}}
	for _, r := range rs {
		if len(f.Items) == maxReadings {
			break
		}
		if !protoID.MatchString(r.ID) {
			continue
		}
		icon := r.Icon
		if !protoID.MatchString(icon) {
			icon = "" // none
		}
		f.Items = append(f.Items, ReadingFrame{
			ID:    r.ID,
			Icon:  icon,
			Label: model.Truncate(r.Label, readingLabelLen),
			Text:  model.Truncate(r.Text, readingTextLen),
			Unit:  model.Truncate(r.Unit, readingUnitLen),
			Sub:   model.Truncate(r.Sub, readingTextLen),
			Pct:   clampPct(r.Pct),
			Level: readingLevel(r.Level),
		})
	}
	return f
}

func clampPct(p *int) *int {
	if p == nil {
		return nil
	}
	v := min(max(*p, 0), 100)
	return &v
}

func readingLevel(l string) string {
	if l == model.LevelWarn || l == model.LevelCrit {
		return l
	}
	return ""
}

// EncodeLine encodes f; maxReadings of them at their longest fit MaxLine.
func EncodeLine(f Line) ([]byte, error) { return encodeLine(f) }

// EncodeHello encodes the host handshake: this computer's name and id,
// and over WiFi the pairing code.
func EncodeHello(name, id, code string) []byte {
	h := Hello{T: "hello", Proto: ProtoVersion, Name: CutName(name), Code: code}
	if ValidHostID(id) {
		h.ID = id
	}
	line, _ := encodeLine(h)
	return line
}

// Firmware update messages (docs/protocol-v1.md §3.4). Unlike the rest of
// the protocol they never change with proto, so a device too old to talk to
// this host can still be updated.
type otaBegin struct {
	T    string `json:"t"`
	Size int    `json:"size"`
	Ver  string `json:"ver"`
}

type otaData struct {
	T    string `json:"t"`
	Off  int    `json:"off"`
	Data []byte `json:"data"` // base64
}

// OTAChunk is the image bytes per ota_data message; base64 keeps the line
// well under MaxLine.
const OTAChunk = 2048

// EncodeOTABegin starts an update with an image of size bytes.
func EncodeOTABegin(size int, version string) []byte {
	line, _ := encodeLine(otaBegin{T: "ota_begin", Size: size, Ver: version})
	return line
}

// EncodeOTAData carries the image bytes b at offset off.
func EncodeOTAData(off int, b []byte) []byte {
	line, _ := encodeLine(otaData{T: "ota_data", Off: off, Data: b})
	return line
}

// EncodeOTA encodes a message without fields: "ota_end" or "ota_abort".
func EncodeOTA(t string) []byte {
	line, _ := encodeLine(struct {
		T string `json:"t"`
	}{t})
	return line
}

// DeviceMsg is any message the device sends; T selects which fields apply.
type DeviceMsg struct {
	T string `json:"t"`
	// hello
	Proto int    `json:"proto"`
	FW    string `json:"fw"`
	W     int    `json:"w"`
	H     int    `json:"h"`
	// ack
	Seq uint64 `json:"seq"`
	// log; also the error text of ota
	Level string `json:"level"`
	Msg   string `json:"msg"`
	// ota: the device's answer during an update
	State string `json:"state"` // ready, ack, done, error
	Off   int    `json:"off"`   // ack: bytes written so far
	// net: the screen's WiFi (State is off, connecting, connected or
	// failed); denied carries its reason in Msg
	SSID  string `json:"ssid"`
	IP    string `json:"ip"`
	Port  int    `json:"port"`
	Code  string `json:"code"`
	Fixed bool   `json:"fixed"`
	Err   string `json:"err"`
	// wifi_list: the networks a scan found, strongest first (and Err)
	APs []AP `json:"aps"`
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
