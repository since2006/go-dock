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

// transcripts tails ~/.claude/projects/**/*.jsonl for token usage and AI
// titles. One API response is written as several lines sharing a message
// id, so usage is kept per message id and each field takes its maximum.
type transcripts struct {
	root   string
	tailer *jsonl.Tailer

	mu       sync.Mutex
	prices   *pricing.Table
	messages map[string]*messageUsage

	agents map[string]*agentTrack // by transcript path; poll goroutine only
	live   func(sessionID string) bool
}

type messageUsage struct {
	at    time.Time
	model string
	u     pricing.Usage
}

type pollResult struct {
	changed bool
	titles  map[string]string // sessionID -> latest AI title
	agents  []model.Observation
}

func newTranscripts(root string, prices *pricing.Table) *transcripts {
	return &transcripts{root: root, tailer: jsonl.NewTailer(), prices: prices, messages: map[string]*messageUsage{}, agents: map[string]*agentTrack{}}
}

func (t *transcripts) setPrices(p *pricing.Table) {
	t.mu.Lock()
	t.prices = p
	t.mu.Unlock()
}

// poll reads new lines. With rescan it also looks for new files.
func (t *transcripts) poll(rescan bool, now time.Time) pollResult {
	res := pollResult{titles: map[string]string{}}
	paths := t.tailer.Paths()
	if rescan {
		paths = jsonl.Walk(t.root, window, func(p string) bool { return strings.HasSuffix(p, ".jsonl") })
	}
	for _, path := range paths {
		agent := t.agentFor(path)
		_ = t.tailer.Read(path, func(line []byte) {
			if t.line(line, res.titles, agent) {
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
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	SessionID string    `json:"sessionId"`
	AITitle   string    `json:"aiTitle"`
	Message   struct {
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
	markAssistant = []byte(`"type":"assistant"`)
	markTitle     = []byte(`"type":"ai-title"`)
)

// line handles one transcript line and reports whether token totals
// changed. agent is set for a subagent's transcript.
func (t *transcripts) line(b []byte, titles map[string]string, agent *agentTrack) bool {
	// Most lines are large tool results; skip them without decoding.
	isAssistant := bytes.Contains(b, markAssistant)
	if !isAssistant && !bytes.Contains(b, markTitle) {
		return false
	}
	var l transcriptLine
	if json.Unmarshal(b, &l) != nil {
		return false
	}
	switch l.Type {
	case "ai-title":
		if l.SessionID != "" && l.AITitle != "" {
			titles[l.SessionID] = model.Truncate(l.AITitle, providers.TitleLen)
		}
		return false
	case "assistant":
	default:
		return false
	}
	if agent != nil {
		agent.toolUse(l.Timestamp, l.Message.Content)
	}
	us := l.Message.Usage
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
