package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/providers"
	"github.com/since2006/go-dock/host/internal/providers/jsonl"
)

const (
	// window is how far back token statistics reach.
	window = 8 * 24 * time.Hour
	// openWithoutLocks treats a recently written rollout as an open session
	// on Codex versions that have no writer lock files.
	openWithoutLocks = 2 * time.Hour
)

// rollouts tails $CODEX_HOME/sessions/**/rollout-*.jsonl for turn state,
// titles, rate limits and token usage.
type rollouts struct {
	home   string
	loc    *time.Location
	tailer *jsonl.Tailer

	threads map[string]*thread       // by rollout path
	days    map[string]*model.Tokens // local date -> tokens
	five    *model.Window
	fiveAt  time.Time
	seven   *model.Window
	sevenAt time.Time
}

// thread is one rollout file's view of a Codex session.
type thread struct {
	id     string
	cwd    string
	hidden bool // sub-agent or internal thread

	state   model.State // empty until the first turn event
	detail  string
	stateAt time.Time
	metaAt  time.Time
	title   string
	last    *tokenUsage // baseline for token deltas

	// What has been sent to the sink.
	open      bool
	sentState model.State
	sentAt    time.Time
	sentTitle string
}

type pollResult struct {
	obs           []model.Observation
	limits        *limits
	tokensChanged bool
}

func newRollouts(home string, loc *time.Location) *rollouts {
	return &rollouts{
		home:    home,
		loc:     loc,
		tailer:  jsonl.NewTailer(),
		threads: map[string]*thread{},
		days:    map[string]*model.Tokens{},
	}
}

func isRollout(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, "rollout-") && strings.HasSuffix(base, ".jsonl")
}

// poll reads new lines and reconciles which sessions are open. With rescan
// it walks the whole sessions tree; otherwise it only looks for new files
// in today's and yesterday's folders.
func (r *rollouts) poll(rescan bool, now time.Time) pollResult {
	var res pollResult
	root := filepath.Join(r.home, "sessions")
	var paths []string
	if rescan {
		paths = jsonl.Walk(root, window, isRollout)
	} else {
		paths = r.tailer.Paths()
		for _, d := range []time.Time{now, now.AddDate(0, 0, -1)} {
			d = d.In(r.loc)
			dir := filepath.Join(root, d.Format("2006"), d.Format("01"), d.Format("02"))
			if m, _ := filepath.Glob(filepath.Join(dir, "rollout-*.jsonl")); len(m) > 0 {
				for _, p := range m {
					if !r.tailer.Known(p) {
						paths = append(paths, p)
					}
				}
			}
		}
	}

	limitsBefore := [2]time.Time{r.fiveAt, r.sevenAt}
	walked := make(map[string]bool, len(paths))
	for _, path := range paths {
		walked[path] = true
		th := r.threads[path]
		if th == nil {
			th = &thread{}
			r.threads[path] = th
		}
		err := r.tailer.Read(path, func(line []byte) {
			if r.line(th, line) {
				res.tokensChanged = true
			}
		})
		if errors.Is(err, fs.ErrNotExist) {
			if th.open {
				res.obs = append(res.obs, model.Observation{Provider: P, SessionID: th.id, End: true, Source: "rollout"})
			}
			delete(r.threads, path)
		}
	}

	res.obs = append(res.obs, r.reconcile(now)...)

	if rescan {
		for path, th := range r.threads {
			if !walked[path] && !th.open {
				delete(r.threads, path)
				r.tailer.Forget(path)
			}
		}
		r.prune(now)
	}
	if r.fiveAt != limitsBefore[0] || r.sevenAt != limitsBefore[1] {
		at := r.fiveAt
		if r.sevenAt.After(at) {
			at = r.sevenAt
		}
		res.limits = &limits{five: r.five, seven: r.seven, at: at, source: "rollout"}
	}
	return res
}

// reconcile emits state for open sessions and ends for closed ones. With a
// thread-writer-locks dir (Codex 0.155+), a session is open while its lock
// file exists. The lock is only checked, never taken.
func (r *rollouts) reconcile(now time.Time) []model.Observation {
	lockDir := filepath.Join(r.home, "thread-writer-locks")
	_, err := os.Stat(lockDir)
	useLocks := err == nil

	var out []model.Observation
	for path, th := range r.threads {
		if th.id == "" || th.hidden {
			continue
		}
		open := false
		if useLocks {
			_, err := os.Stat(filepath.Join(lockDir, th.id+".lock"))
			open = err == nil
		} else if fi, err := os.Stat(path); err == nil {
			open = now.Sub(fi.ModTime()) < openWithoutLocks
		}
		if !open {
			if th.open {
				out = append(out, model.Observation{Provider: P, SessionID: th.id, End: true, Source: "rollout"})
				th.open, th.sentState, th.sentAt, th.sentTitle = false, "", time.Time{}, ""
			}
			continue
		}
		st, at := th.state, th.stateAt
		if st == "" {
			st, at = model.StateIdle, th.metaAt
		}
		switch {
		case !th.open || st != th.sentState || !at.Equal(th.sentAt):
			out = append(out, model.Observation{
				Provider: P, SessionID: th.id, Cwd: th.cwd, Title: th.title,
				State: st, Detail: th.detail, At: at, Source: "rollout",
			})
			th.open, th.sentState, th.sentAt, th.sentTitle = true, st, at, th.title
		case th.title != th.sentTitle:
			out = append(out, model.Observation{Provider: P, SessionID: th.id, Title: th.title, Source: "rollout"})
			th.sentTitle = th.title
		}
	}
	return out
}

type rolloutLine struct {
	Timestamp time.Time       `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type sessionMeta struct {
	ID           string          `json:"id"`
	SessionID    string          `json:"session_id"`
	Cwd          string          `json:"cwd"`
	Timestamp    time.Time       `json:"timestamp"`
	Source       json.RawMessage `json:"source"`
	ThreadSource string          `json:"thread_source"`
	ParentThread *string         `json:"parent_thread_id"`
}

type eventMsg struct {
	Type       string          `json:"type"`
	Message    string          `json:"message"` // user_message
	Item       *completedItem  `json:"item"`    // item_completed
	Info       *tokenInfo      `json:"info"`    // token_count
	RateLimits json.RawMessage `json:"rate_limits"`
}

type completedItem struct {
	Type    string `json:"type"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
}

type tokenInfo struct {
	Total *tokenUsage `json:"total_token_usage"`
}

type tokenUsage struct {
	Input      int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
	Output     int64 `json:"output_tokens"`
	Total      int64 `json:"total_tokens"`
}

var (
	markEvent = []byte(`"type":"event_msg"`)
	markMeta  = []byte(`"type":"session_meta"`)
	markTurn  = []byte(`"type":"turn_context"`)
)

// line handles one rollout line and reports whether token totals changed.
func (r *rollouts) line(th *thread, b []byte) bool {
	// response_item lines carry whole model outputs; skip them undecoded.
	if !bytes.Contains(b, markEvent) && !bytes.Contains(b, markMeta) && !bytes.Contains(b, markTurn) {
		return false
	}
	var l rolloutLine
	if json.Unmarshal(b, &l) != nil {
		return false
	}
	switch l.Type {
	case "session_meta":
		var m sessionMeta
		if json.Unmarshal(l.Payload, &m) != nil {
			return false
		}
		if th.id == "" {
			th.id = m.ID
			if th.id == "" {
				th.id = m.SessionID
			}
			th.metaAt = m.Timestamp
			if th.metaAt.IsZero() {
				th.metaAt = l.Timestamp
			}
			th.hidden = hiddenThread(m)
		}
		if m.Cwd != "" {
			th.cwd = m.Cwd
		}
	case "turn_context":
		var c struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(l.Payload, &c) == nil && c.Cwd != "" {
			th.cwd = c.Cwd
		}
	case "event_msg":
		var e eventMsg
		if json.Unmarshal(l.Payload, &e) != nil {
			return false
		}
		return r.event(th, l.Timestamp, e)
	}
	return false
}

func hiddenThread(m sessionMeta) bool {
	if m.ParentThread != nil && *m.ParentThread != "" {
		return true
	}
	switch m.ThreadSource {
	case "subagent", "guardian_review", "memory_consolidation":
		return true
	}
	var src map[string]json.RawMessage
	if json.Unmarshal(m.Source, &src) == nil {
		_, sub := src["subagent"]
		_, internal := src["internal"]
		return sub || internal
	}
	return false
}

func (r *rollouts) event(th *thread, ts time.Time, e eventMsg) bool {
	switch e.Type {
	case "task_started", "turn_started":
		th.state, th.detail, th.stateAt = model.StateRunning, "", ts
	case "task_complete", "turn_complete":
		th.state, th.detail, th.stateAt = model.StateDone, "", ts
	case "turn_aborted":
		th.state, th.detail, th.stateAt = model.StateIdle, "已中断", ts
	case "user_message":
		if t := model.Truncate(e.Message, providers.TitleLen); t != "" {
			th.title = t
		}
	case "item_completed":
		if e.Item != nil && e.Item.Type == "UserMessage" {
			for _, c := range e.Item.Content {
				if t := model.Truncate(c.Text, providers.TitleLen); t != "" {
					th.title = t
					break
				}
			}
		}
	case "token_count":
		r.rateLimits(ts, e.RateLimits)
		if e.Info != nil && e.Info.Total != nil {
			return r.tokens(th, ts, *e.Info.Total)
		}
	}
	return false
}

// tokens attributes the growth of a thread's cumulative usage to the day
// of the event. A drop in the total restarts the baseline.
func (r *rollouts) tokens(th *thread, ts time.Time, cur tokenUsage) bool {
	last := th.last
	th.last = &cur
	if last == nil {
		last = &tokenUsage{}
	} else if cur.Total < last.Total {
		return false
	}
	d := model.Tokens{
		Input:       nonNeg((cur.Input - cur.Cached) - (last.Input - last.Cached)),
		CacheRead:   nonNeg(cur.Cached - last.Cached),
		Output:      nonNeg(cur.Output - last.Output),
		CacheCreate: nonNeg(cur.CacheWrite - last.CacheWrite),
	}
	if d.Total() == 0 || ts.IsZero() {
		return false
	}
	key := ts.In(r.loc).Format(time.DateOnly)
	t := r.days[key]
	if t == nil {
		t = &model.Tokens{}
		r.days[key] = t
	}
	t.Add(d)
	return true
}

func nonNeg(v int64) int64 { return max(v, 0) }

func (r *rollouts) prune(now time.Time) {
	cutoff := now.In(r.loc).Add(-window).Format(time.DateOnly)
	for k := range r.days {
		if k < cutoff {
			delete(r.days, k)
		}
	}
}

// totals sums today and the last 7 local days including today.
func (r *rollouts) totals(now time.Time) (today, week model.Tokens) {
	now = now.In(r.loc)
	for i := 0; i < 7; i++ {
		t := r.days[now.AddDate(0, 0, -i).Format(time.DateOnly)]
		if t == nil {
			continue
		}
		week.Add(*t)
		if i == 0 {
			today.Add(*t)
		}
	}
	return today, week
}

type rateLimits struct {
	LimitID   *string     `json:"limit_id"`
	Primary   *rateWindow `json:"primary"`
	Secondary *rateWindow `json:"secondary"`
	// Flat fields written by Codex 0.40 and earlier.
	PrimaryUsedPercent     *float64 `json:"primary_used_percent"`
	SecondaryUsedPercent   *float64 `json:"secondary_used_percent"`
	PrimaryWindowMinutes   *int     `json:"primary_window_minutes"`
	SecondaryWindowMinutes *int     `json:"secondary_window_minutes"`
}

type rateWindow struct {
	UsedPercent     *float64 `json:"used_percent"`
	WindowMinutes   *int     `json:"window_minutes"`
	ResetsAt        *int64   `json:"resets_at"`         // unix seconds, 0.48+
	ResetsInSeconds *int64   `json:"resets_in_seconds"` // 0.41-0.47
}

func (w *rateWindow) window(ts time.Time, defMinutes int) *model.Window {
	if w == nil || w.UsedPercent == nil {
		return nil
	}
	out := &model.Window{UsedPct: *w.UsedPercent, WindowMinutes: defMinutes}
	if w.WindowMinutes != nil && *w.WindowMinutes > 0 {
		out.WindowMinutes = *w.WindowMinutes
	}
	switch {
	case w.ResetsAt != nil && *w.ResetsAt > 0:
		out.ResetsAt = time.Unix(*w.ResetsAt, 0)
	case w.ResetsInSeconds != nil && !ts.IsZero():
		out.ResetsAt = ts.Add(time.Duration(*w.ResetsInSeconds) * time.Second).Truncate(time.Second)
	}
	return out
}

// parseRateLimits reads a token_count rate_limits object. Buckets other
// than the main "codex" one are ignored.
func parseRateLimits(raw json.RawMessage, ts time.Time) (five, seven *model.Window) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var rl rateLimits
	if json.Unmarshal(raw, &rl) != nil {
		return nil, nil
	}
	if rl.LimitID != nil && *rl.LimitID != "" && *rl.LimitID != "codex" {
		return nil, nil
	}
	five = rl.Primary.window(ts, 300)
	seven = rl.Secondary.window(ts, 10080)
	if five == nil && rl.PrimaryUsedPercent != nil {
		five = &model.Window{UsedPct: *rl.PrimaryUsedPercent, WindowMinutes: orDefault(rl.PrimaryWindowMinutes, 300)}
	}
	if seven == nil && rl.SecondaryUsedPercent != nil {
		seven = &model.Window{UsedPct: *rl.SecondaryUsedPercent, WindowMinutes: orDefault(rl.SecondaryWindowMinutes, 10080)}
	}
	return five, seven
}

func orDefault(v *int, def int) int {
	if v != nil && *v > 0 {
		return *v
	}
	return def
}

// rateLimits keeps the newest reading of each window across all files.
func (r *rollouts) rateLimits(ts time.Time, raw json.RawMessage) {
	five, seven := parseRateLimits(raw, ts)
	if five != nil && !ts.Before(r.fiveAt) {
		r.five, r.fiveAt = five, ts
	}
	if seven != nil && !ts.Before(r.sevenAt) {
		r.seven, r.sevenAt = seven, ts
	}
}
