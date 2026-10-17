package claude

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/providers"
	"github.com/since2006/go-dock/host/internal/providers/jsonl"
	"github.com/since2006/go-dock/host/internal/providers/pricing"
)

// window is how far back token statistics reach.
const window = 8 * 24 * time.Hour

// transcripts tails ~/.claude/projects/**/*.jsonl for token usage, session
// titles and the assistant's latest text. One API response is written as several lines sharing a
// message id, so usage is kept per message id and each field takes its
// maximum.
type transcripts struct {
	root   string
	tailer *jsonl.Tailer

	mu       sync.Mutex
	prices   *pricing.Table
	messages map[string]*messageUsage

	agents map[string]*agentTrack // by transcript path; poll goroutine only
	live   func(sessionID string) bool
	info   map[string]*sessionInfo // by session id; poll goroutine only
}

// sessionInfo is what a main transcript says about its session: the two
// titles Claude Code keeps (the /rename one wins over the generated one)
// and the current turn.
type sessionInfo struct {
	ai, custom string
	turn       model.Turn
	outputs    map[string]int64 // message id -> output tokens, this turn
}

func (n *sessionInfo) title() string {
	if n.custom != "" {
		return n.custom
	}
	return n.ai
}

type messageUsage struct {
	at    time.Time
	model string
	u     pricing.Usage
}

type pollResult struct {
	changed bool
	titles  map[string]string     // sessionID -> title, for sessions whose title lines were read
	turns   map[string]model.Turn // sessionID -> current turn, for sessions whose turn changed
	agents  []model.Observation
}

func newTranscripts(root string, prices *pricing.Table) *transcripts {
	return &transcripts{root: root, tailer: jsonl.NewTailer(), prices: prices, messages: map[string]*messageUsage{}, agents: map[string]*agentTrack{}, info: map[string]*sessionInfo{}}
}

func (t *transcripts) setPrices(p *pricing.Table) {
	t.mu.Lock()
	t.prices = p
	t.mu.Unlock()
}

// poll reads new lines. With rescan it also looks for new files.
func (t *transcripts) poll(rescan bool, now time.Time) pollResult {
	res := pollResult{titles: map[string]string{}, turns: map[string]model.Turn{}}
	paths := t.tailer.Paths()
	if rescan {
		paths = jsonl.Walk(t.root, window, func(p string) bool { return strings.HasSuffix(p, ".jsonl") })
	}
	for _, path := range paths {
		agent := t.agentFor(path)
		_ = t.tailer.Read(path, func(line []byte) {
			if t.line(line, &res, agent) {
				res.changed = true
			}
		})
	}
	res.agents = t.agentObservations(now, rescan)
	if rescan {
		t.prune(now)
	}
	return res
}

type transcriptLine struct {
	Type        string    `json:"type"`
	Timestamp   time.Time `json:"timestamp"`
	SessionID   string    `json:"sessionId"`
	AITitle     string    `json:"aiTitle"`
	CustomTitle string    `json:"customTitle"`
	IsMeta      bool      `json:"isMeta"`
	IsSidechain bool      `json:"isSidechain"`
	Message     struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			Input         int64 `json:"input_tokens"`
			Output        int64 `json:"output_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
			Split         *struct {
				FiveMin int64 `json:"ephemeral_5m_input_tokens"`
				OneHour int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	} `json:"message"`
}

var (
	markAssistant   = []byte(`"type":"assistant"`)
	markUser        = []byte(`"type":"user"`)
	markToolResult  = []byte(`"tool_use_id"`)
	markTitle       = []byte(`"type":"ai-title"`)
	markCustomTitle = []byte(`"type":"custom-title"`)
)

// line handles one transcript line and reports whether token totals
// changed. agent is set for a subagent's transcript.
func (t *transcripts) line(b []byte, res *pollResult, agent *agentTrack) bool {
	// Most lines are large tool results; skip them without decoding.
	isAssistant := bytes.Contains(b, markAssistant)
	isPrompt := agent == nil && bytes.Contains(b, markUser) && !bytes.Contains(b, markToolResult)
	if !isAssistant && !isPrompt && !bytes.Contains(b, markTitle) && !bytes.Contains(b, markCustomTitle) {
		return false
	}
	var l transcriptLine
	if json.Unmarshal(b, &l) != nil {
		return false
	}
	switch l.Type {
	case "ai-title", "custom-title":
		if l.SessionID == "" {
			return false
		}
		n := t.session(l.SessionID)
		if l.Type == "ai-title" && l.AITitle != "" {
			n.ai = model.Truncate(l.AITitle, providers.TitleLen)
		}
		if l.Type == "custom-title" && l.CustomTitle != "" {
			n.custom = model.Truncate(l.CustomTitle, providers.TitleLen)
		}
		if title := n.title(); title != "" {
			res.titles[l.SessionID] = title
		}
		return false
	case "user":
		// A new prompt starts a turn: the previous reply no longer applies.
		if agent == nil && !l.IsMeta && !l.IsSidechain && userPrompt(l.Message.Content) {
			t.updateTurn(res, l.SessionID, func(n *sessionInfo) { n.turn, n.outputs = model.Turn{}, nil })
		}
		return false
	case "assistant":
	default:
		return false
	}
	us := l.Message.Usage
	if agent != nil {
		agent.toolUse(l.Timestamp, l.Message.Content)
	} else if !l.IsSidechain {
		t.updateTurn(res, l.SessionID, func(n *sessionInfo) {
			if msg := assistantText(l.Message.Content); msg != "" {
				n.turn.Message = msg
			}
			// Lines of one response repeat its usage; count each id once.
			if id := l.Message.ID; us != nil && id != "" && us.Output > n.outputs[id] {
				if n.outputs == nil {
					n.outputs = map[string]int64{}
				}
				n.turn.Output += us.Output - n.outputs[id]
				n.outputs[id] = us.Output
			}
		})
	}
	if us == nil || l.Message.ID == "" || l.Timestamp.IsZero() {
		return false
	}
	u := pricing.Usage{Input: us.Input, Output: us.Output, CacheRead: us.CacheRead}
	if us.Split != nil && us.Split.FiveMin+us.Split.OneHour > 0 {
		u.CacheWrite5m, u.CacheWrite1h = us.Split.FiveMin, us.Split.OneHour
	} else {
		u.CacheWrite5m = us.CacheCreation
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	m, ok := t.messages[l.Message.ID]
	if !ok {
		t.messages[l.Message.ID] = &messageUsage{at: l.Timestamp, model: l.Message.Model, u: u}
		return true
	}
	before := m.u
	m.u.Input = max(m.u.Input, u.Input)
	m.u.Output = max(m.u.Output, u.Output)
	m.u.CacheRead = max(m.u.CacheRead, u.CacheRead)
	m.u.CacheWrite5m = max(m.u.CacheWrite5m, u.CacheWrite5m)
	m.u.CacheWrite1h = max(m.u.CacheWrite1h, u.CacheWrite1h)
	return m.u != before
}

func (t *transcripts) session(id string) *sessionInfo {
	n := t.info[id]
	if n == nil {
		n = &sessionInfo{}
		t.info[id] = n
	}
	return n
}

func (t *transcripts) updateTurn(res *pollResult, id string, f func(*sessionInfo)) {
	if id == "" {
		return
	}
	n := t.session(id)
	before := n.turn
	f(n)
	if n.turn != before {
		res.turns[id] = n.turn
	}
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// userPrompt reports whether a user line's content is something the user
// typed, as opposed to a tool result, a slash command's echo or an
// interruption notice.
func userPrompt(content json.RawMessage) bool {
	var text string
	if json.Unmarshal(content, &text) != nil {
		var blocks []contentBlock
		if json.Unmarshal(content, &blocks) != nil {
			return false
		}
		for _, b := range blocks {
			if b.Type == "tool_result" {
				return false
			}
			if b.Type == "text" && text == "" {
				text = b.Text
			}
		}
	}
	text = strings.TrimSpace(text)
	for _, p := range []string{"<command-name>", "<command-message>", "<local-command-", "[Request interrupted"} {
		if strings.HasPrefix(text, p) {
			return false
		}
	}
	return text != ""
}

// assistantText returns the display line of the last text block in an
// assistant line's content, or "".
func assistantText(content json.RawMessage) string {
	var blocks []contentBlock
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		if blocks[i].Type == "text" {
			if msg := providers.MessageLine(blocks[i].Text); msg != "" {
				return msg
			}
		}
	}
	return ""
}

func (t *transcripts) prune(now time.Time) {
	cutoff := now.Add(-window)
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, m := range t.messages {
		if m.at.Before(cutoff) {
			delete(t.messages, id)
		}
	}
}

// totals sums today (local time) and the last 7 days including today.
func (t *transcripts) totals(now time.Time) (today, week model.Tokens) {
	y, mo, d := now.Date()
	dayStart := time.Date(y, mo, d, 0, 0, 0, 0, now.Location())
	weekStart := dayStart.AddDate(0, 0, -6)

	t.mu.Lock()
	defer t.mu.Unlock()
	for _, m := range t.messages {
		if m.at.Before(weekStart) {
			continue
		}
		tk := model.Tokens{
			Input:       m.u.Input,
			Output:      m.u.Output,
			CacheRead:   m.u.CacheRead,
			CacheCreate: m.u.CacheWrite5m + m.u.CacheWrite1h,
		}
		if cost, ok := t.prices.Cost(m.model, m.u); ok {
			tk.CostUSD, tk.HasCost = cost, true
		}
		week.Add(tk)
		if !m.at.Before(dayStart) {
			today.Add(tk)
		}
	}
	return today, week
}
