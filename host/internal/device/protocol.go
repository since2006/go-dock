// Package device talks to the ESP32-S3 display over USB serial using the
// newline-delimited JSON protocol in docs/protocol-v1.md.
package device

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
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
	textLen    = 48
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

	// Function pages (§3.5): what one screen page holds.
	maxPages     = 4
	maxItems     = 6
	pageTitleLen = 8
	pageNoteLen  = 24 // summary and error
	itemLabelLen = 12
	itemTextLen  = 8 // also a meter's sub and a row's text
	itemUnitLen  = 4
	itemNoteLen  = 16
	maxHist      = 30
	maxSeries    = 3
	maxRows      = 4
	rowLabelLen  = 6
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

// Pages carries the function pages shown next to the Agent page
// (docs/protocol-v1.md §3.5).
type Pages struct {
	T     string      `json:"t"`
	Pages []PageFrame `json:"pages"`
}

type PageFrame struct {
	ID      string      `json:"id"`
	Title   string      `json:"title"`
	Summary string      `json:"summary,omitempty"`
	Error   string      `json:"error,omitempty"`
	Movable bool        `json:"movable,omitempty"`
	Items   []ItemFrame `json:"items"`
}

// ItemFrame is one widget; On is omitted while a switch's state is
// unknown.
type ItemFrame struct {
	ID       string        `json:"id"`
	Kind     string        `json:"kind"`
	Label    string        `json:"label"`
	Text     string        `json:"text"`
	Icon     string        `json:"icon,omitempty"`
	On       *bool         `json:"on,omitempty"`
	Buttons  bool          `json:"buttons,omitempty"`
	Busy     bool          `json:"busy,omitempty"`
	Disabled bool          `json:"disabled,omitempty"`
	AtMin    bool          `json:"at_min,omitempty"`
	AtMax    bool          `json:"at_max,omitempty"`
	Unit     string        `json:"unit,omitempty"`
	Sub      string        `json:"sub,omitempty"`
	Pct      *int          `json:"pct,omitempty"`
	Hist     []int         `json:"hist,omitempty"`
	Level    string        `json:"level,omitempty"`
	Big      bool          `json:"big,omitempty"`
	Note     string        `json:"note,omitempty"`
	Series   []SeriesFrame `json:"series,omitempty"`
	Rows     []RowFrame    `json:"rows,omitempty"`
}

type SeriesFrame struct {
	C    string `json:"c"`
	Hist []int  `json:"hist"`
}

type RowFrame struct {
	Label string `json:"label"`
	Text  string `json:"text"`
	C     string `json:"c"`
}

// protoID is what page, item and kind ids may look like, so the firmware
// can echo them back in an action without escaping.
var protoID = regexp.MustCompile(`^[a-z0-9_-]{1,16}$`)

// BuildPages converts plugin pages into the pages message: at most
// maxPages pages of maxItems items, texts cut to fit the screen, and
// anything with an unusable id dropped.
func BuildPages(ps []model.Page) Pages {
	f := Pages{T: "pages", Pages: []PageFrame{}}
	for _, p := range ps {
		if len(f.Pages) == maxPages {
			break
		}
		if !protoID.MatchString(p.ID) {
			continue
		}
		pf := PageFrame{
			ID:      p.ID,
			Title:   model.Truncate(p.Title, pageTitleLen),
			Summary: model.Truncate(p.Summary, pageNoteLen),
			Error:   model.Truncate(p.Error, pageNoteLen),
			Movable: p.Movable,
			Items:   []ItemFrame{},
		}
		for _, it := range p.Items {
			if len(pf.Items) == maxItems {
				break
			}
			if !protoID.MatchString(it.ID) || !protoID.MatchString(it.Kind) {
				continue
			}
			icon := it.Icon
			if !protoID.MatchString(icon) {
				icon = "" // the default picture
			}
			pf.Items = append(pf.Items, ItemFrame{
				ID:       it.ID,
				Kind:     it.Kind,
				Label:    model.Truncate(it.Label, itemLabelLen),
				Text:     model.Truncate(it.Text, itemTextLen),
				Icon:     icon,
				On:       it.On,
				Buttons:  it.Buttons,
				Busy:     it.Busy,
				Disabled: it.Disabled,
				AtMin:    it.AtMin,
				AtMax:    it.AtMax,
				Unit:     model.Truncate(it.Unit, itemUnitLen),
				Sub:      model.Truncate(it.Sub, itemTextLen),
				Pct:      clampPct(it.Pct),
				Hist:     meterHist(it.Hist),
				Level:    meterLevel(it.Level),
				Big:      it.Big,
				Note:     model.Truncate(it.Note, itemNoteLen),
				Series:   meterSeries(it.Series),
				Rows:     meterRows(it.Rows),
			})
		}
		f.Pages = append(f.Pages, pf)
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

// meterHist keeps the last maxHist readings, each 0–100.
func meterHist(h []int) []int {
	if len(h) == 0 {
		return nil
	}
	h = h[max(len(h)-maxHist, 0):]
	out := make([]int, len(h))
	for i, v := range h {
		out[i] = min(max(v, 0), 100)
	}
	return out
}

var colors = map[string]bool{
	model.ColorBlue: true, model.ColorRed: true, model.ColorTeal: true, model.ColorGreen: true,
	model.ColorOrange: true, model.ColorPurple: true, model.ColorGray: true,
}

// meterColor keeps the colours the screen knows; "" draws in the card's.
func meterColor(c string) string {
	if colors[c] {
		return c
	}
	return ""
}

func meterSeries(ss []model.Series) []SeriesFrame {
	var out []SeriesFrame
	for _, s := range ss[:min(len(ss), maxSeries)] {
		if h := meterHist(s.Hist); len(h) > 0 {
			out = append(out, SeriesFrame{C: meterColor(s.Color), Hist: h})
		}
	}
	return out
}

func meterRows(rs []model.Row) []RowFrame {
	var out []RowFrame
	for _, r := range rs[:min(len(rs), maxRows)] {
		out = append(out, RowFrame{Label: model.Truncate(r.Label, rowLabelLen), Text: model.Truncate(r.Text, itemTextLen), C: meterColor(r.Color)})
	}
	return out
}

func meterLevel(l string) string {
	if l == model.LevelWarn || l == model.LevelCrit {
		return l
	}
	return ""
}

// EncodePages encodes f as one line of at most MaxLine bytes, dropping
// items and then pages from the tail when it is too long.
func EncodePages(f Pages) ([]byte, error) {
	f.Pages = slices.Clone(f.Pages) // trimmed below
	for {
		line, err := encodeLine(f)
		if err != nil {
			return nil, err
		}
		if len(line) <= MaxLine {
			return line, nil
		}
		last := &f.Pages[len(f.Pages)-1] // an empty list always fits
		if n := len(last.Items); n > 0 {
			last.Items = last.Items[: n-1 : n-1]
		} else {
			f.Pages = f.Pages[:len(f.Pages)-1]
		}
	}
}

// EncodeHello encodes the host handshake.
func EncodeHello(hostVersion string) []byte {
	line, _ := encodeLine(Hello{T: "hello", Proto: ProtoVersion, Host: "go-dock", Ver: hostVersion})
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
	Proto   int    `json:"proto"`
	FW      string `json:"fw"`
	W       int    `json:"w"`
	H       int    `json:"h"`
	MaxLine int    `json:"max_line"`
	OTA     int    `json:"ota"`   // 1: can be updated over the link
	Pages   int    `json:"pages"` // 1: shows function pages (§3.5)
	Beep    int    `json:"beep"`  // 1: plays beep messages (§3.6); 2: at their vol
	IR      int    `json:"ir"`    // 1: sends infrared (§3.7)
	// 1: takes brightness settings (§3.3)
	Settings int `json:"settings"`
	// ack
	Seq uint64 `json:"seq"`
	// log; also the error text of ota
	Level string `json:"level"`
	Msg   string `json:"msg"`
	// ota: the device's answer during an update
	State string `json:"state"` // ready, ack, done, error
	Off   int    `json:"off"`   // ack: bytes written so far
	// action: a tap on a function page item
	Page string `json:"page"`
	Item string `json:"item"`
	Act  string `json:"act"`
	To   string `json:"to"` // swap: the item dropped on
	// ir_recv: what the infrared receiver picked up, µs
	Raw []int `json:"raw"`
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
