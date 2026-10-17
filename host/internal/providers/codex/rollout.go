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
// tool calls, messages, rate limits and token usage, and
// $CODEX_HOME/session_index.jsonl for thread names.
type rollouts struct {
	home   string
	loc    *time.Location
	tailer *jsonl.Tailer
	index  *jsonl.Tailer     // session_index.jsonl
	names  map[string]string // thread id -> name (/rename or generated)

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
	hidden bool   // internal thread (guardian review, title generation, ...)
	parent string // for a spawned subagent: the root session it works for
	role   string // subagent role, e.g. "explorer", else its nickname

	state    model.State // empty until the first turn event
	detail   string      // the latest tool call of the turn
	stateAt  time.Time
	detailAt time.Time
	asking   string // call id of a request_user_input waiting for the user
	metaAt   time.Time
	name     string      // from thread_name_updated (Codex 0.128 and older)
	prompt   string      // the first user message
	task     string      // the latest user message: a subagent's task
	turn     model.Turn  // latest message and output tokens of the current turn
	last     *tokenUsage // baseline for token deltas

	// What has been sent to the sink.
	open       bool
	sentState  model.State
	sentAt     time.Time
	sentDetail string
	sentTitle  string
	sentTurn   model.Turn
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
		index:   jsonl.NewTailer(),
		names:   map[string]string{},
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

	_ = r.index.Read(filepath.Join(r.home, "session_index.jsonl"), r.indexLine)
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
				end := model.Observation{Provider: P, SessionID: th.id, End: true, Source: "rollout"}
				if th.parent != "" {
					end.SessionID, end.AgentID = th.parent, th.id
				}
				out = append(out, end)
				th.open, th.sentState, th.sentAt, th.sentDetail, th.sentTitle, th.sentTurn = false, "", time.Time{}, "", "", model.Turn{}
			}
			continue
		}
		st, at := th.state, th.stateAt
		if st == "" {
			st, at = model.StateIdle, th.metaAt
		}
		if th.parent != "" {
			if o, ok := th.agentObservation(st, at); ok {
				out = append(out, o)
			}
			continue
		}
		title, turn := r.title(th), th.turn
		switch {
		case !th.open || st != th.sentState || !at.Equal(th.sentAt):
			out = append(out, model.Observation{
				Provider: P, SessionID: th.id, Cwd: th.cwd, Title: title, Turn: &turn,
				State: st, Detail: th.detail, At: at, Source: "rollout",
			})
		case th.detail != th.sentDetail:
			// A new tool call in the same state, as of when it was made.
			out = append(out, model.Observation{
				Provider: P, SessionID: th.id, Title: title, Turn: &turn,
				State: st, Detail: th.detail, At: th.detailAt, Source: "rollout",
			})
		case title != th.sentTitle || turn != th.sentTurn:
			out = append(out, model.Observation{Provider: P, SessionID: th.id, Title: title, Turn: &turn, Source: "rollout"})
		default:
			continue
		}
		th.open, th.sentState, th.sentAt, th.sentDetail, th.sentTitle, th.sentTurn = true, st, at, th.detail, title, turn
	}
	return out
}

// title names a thread as Codex does: by its name (set with /rename, or
// generated after the first prompt), else by its first prompt.
func (r *rollouts) title(th *thread) string {
	if n := r.names[th.id]; n != "" {
		return n
	}
	if th.name != "" {
		return th.name
	}
	return th.prompt
}

// indexLine reads one session_index.jsonl entry. Codex appends one per
// rename; the last one per thread wins and empty names are skipped.
func (r *rollouts) indexLine(b []byte) {
	var e struct {
		ID   string `json:"id"`
		Name string `json:"thread_name"`
	}
	if json.Unmarshal(b, &e) == nil && e.ID != "" {
		if n := model.Truncate(e.Name, providers.TitleLen); n != "" {
			r.names[e.ID] = n
		}
	}
}

// agentObservation reports a subagent thread as an agent of its session.
// An agent that has not started a turn yet counts as running.
func (th *thread) agentObservation(st model.State, at time.Time) (model.Observation, bool) {
	switch st {
	case model.StateIdle:
		if th.state == "" {
			st = model.StateRunning
		} else {
			st = model.StateDone // interrupted
		}
	case model.StateDone, model.StateRunning, model.StateInput:
	default:
		st = model.StateRunning
	}
	same := th.open && st == th.sentState && at.Equal(th.sentAt)
	if same && th.detail == th.sentDetail && th.task == th.sentTitle {
		return model.Observation{}, false
	}
	obsAt := at
	if same && th.detailAt.After(at) {
		obsAt = th.detailAt // a new tool call
	}
	th.open, th.sentState, th.sentAt, th.sentDetail, th.sentTitle = true, st, at, th.detail, th.task
	return model.Observation{
		Provider: P, SessionID: th.parent, Cwd: th.cwd, At: obsAt, Source: "rollout",
		AgentID: th.id, AgentType: th.role, AgentDesc: th.task, State: st, Detail: th.detail,
	}, true
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
	AgentRole    string          `json:"agent_role"`
	AgentNick    string          `json:"agent_nickname"`
}

type eventMsg struct {
	Type       string          `json:"type"`
	Message    string          `json:"message"`            // user_message, agent_message
	LastAgent  string          `json:"last_agent_message"` // task_complete
	Error      json.RawMessage `json:"error"`              // task_complete of a failed turn
	ThreadName string          `json:"thread_name"`        // thread_name_updated
	Item       *completedItem  `json:"item"`               // item_completed
	Info       *tokenInfo      `json:"info"`               // token_count
	RateLimits json.RawMessage `json:"rate_limits"`
}

// errorText reads a failed turn's error, {"message": ...}; a plain
// string is taken as is.
func errorText(raw json.RawMessage) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) != nil {
		_ = json.Unmarshal(raw, &e.Message)
	}
	return model.Truncate(e.Message, providers.DetailLen)
}

// responseItem is a tool call the model made (function_call,
// custom_tool_call, local_shell_call, web_search_call) or a tool's output.
type responseItem struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Arguments json.RawMessage `json:"arguments"` // function_call: JSON text
	Input     string          `json:"input"`     // custom_tool_call
	Action    json.RawMessage `json:"action"`    // local_shell_call, web_search_call
	CallID    string          `json:"call_id"`
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
	markEvent    = []byte(`"type":"event_msg"`)
	markMeta     = []byte(`"type":"session_meta"`)
	markTurn     = []byte(`"type":"turn_context"`)
	markResponse = []byte(`"type":"response_item"`)
	markCalls    = [][]byte{
		[]byte(`"type":"function_call"`), []byte(`"type":"custom_tool_call"`),
		[]byte(`"type":"local_shell_call"`), []byte(`"type":"web_search_call"`),
	}
)

// line handles one rollout line and reports whether token totals changed.
func (r *rollouts) line(th *thread, b []byte) bool {
	if !bytes.Contains(b, markEvent) && !bytes.Contains(b, markMeta) && !bytes.Contains(b, markTurn) && !toolLine(th, b) {
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
			th.hidden = internalThread(m)
			if !th.hidden {
				th.parent = subagentParent(m)
				// Agents without a role are "default"; Codex tells them
				// apart by nickname.
				th.role = m.AgentRole
				if (th.role == "" || th.role == "default") && m.AgentNick != "" {
					th.role = m.AgentNick
				}
				if th.role == "" && th.parent != "" {
					th.role = "subagent"
				}
			}
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
	case "response_item":
		var it responseItem
		if json.Unmarshal(l.Payload, &it) == nil {
			th.toolItem(l.Timestamp, it)
		}
	}
	return false
}

// toolLine reports a response_item line worth decoding: a tool call, or
// the answer to a pending request_user_input. Other response items carry
// whole model outputs and are skipped undecoded.
func toolLine(th *thread, b []byte) bool {
	if !bytes.Contains(b, markResponse) {
		return false
	}
	if th.asking != "" && bytes.Contains(b, []byte(th.asking)) {
		return true
	}
	for _, m := range markCalls {
		if bytes.Contains(b, m) {
			return true
		}
	}
	return false
}

// toolItem follows the tool calls of a running turn. Codex writes a call
// before running it, so the latest call is what the thread is doing now.
func (th *thread) toolItem(ts time.Time, it responseItem) {
	if it.Type == "function_call_output" || it.Type == "custom_tool_call_output" {
		if th.asking != "" && it.CallID == th.asking {
			th.asking = ""
			if th.state == model.StateInput {
				th.state, th.detail, th.stateAt = model.StateRunning, "", ts
			}
		}
		return
	}
	if th.state != model.StateRunning {
		return
	}
	detail := ""
	switch it.Type {
	case "function_call":
		name := toolName(it.Namespace, it.Name)
		if name == "request_user_input" {
			th.state, th.detail, th.stateAt, th.asking = model.StateInput, "等待回答问题", ts, it.CallID
			return
		}
		args := it.Arguments
		var text string
		if json.Unmarshal(args, &text) == nil {
			args = json.RawMessage(text)
		}
		detail = toolDetail(name, args)
	case "custom_tool_call":
		in, _ := json.Marshal(map[string]string{"command": it.Input})
		detail = toolDetail(it.Name, in)
	case "local_shell_call":
		detail = providers.ToolSummary("Bash", it.Action)
	case "web_search_call":
		detail = providers.ToolSummary("web_search", it.Action)
	}
	if detail != "" {
		th.detail, th.detailAt = detail, ts
	}
}

// internalThread reports Codex's own helper threads, which go-dock hides.
func internalThread(m sessionMeta) bool {
	switch m.ThreadSource {
	case "guardian_review", "memory_consolidation", "thread_title", "system":
		return true
	}
	var src map[string]json.RawMessage
	if json.Unmarshal(m.Source, &src) == nil {
		_, internal := src["internal"]
		return internal
	}
	return false
}

// subagentParent returns the session a spawned subagent thread belongs to,
// or "" for a top-level thread. All threads of one session share its
// session_id; older versions only link the direct parent.
func subagentParent(m sessionMeta) string {
	sub := m.ThreadSource == "subagent" || (m.ParentThread != nil && *m.ParentThread != "")
	var src map[string]json.RawMessage
	if json.Unmarshal(m.Source, &src) == nil {
		_, ok := src["subagent"]
		sub = sub || ok
	}
	if !sub {
		return ""
	}
	if m.SessionID != "" && m.SessionID != m.ID {
		return m.SessionID
	}
	if m.ParentThread != nil {
		return *m.ParentThread
	}
	return ""
}

func (r *rollouts) event(th *thread, ts time.Time, e eventMsg) bool {
	switch e.Type {
	case "task_started", "turn_started":
		th.state, th.detail, th.stateAt, th.asking, th.turn = model.StateRunning, "", ts, "", model.Turn{}
	case "agent_message":
		th.message(e.Message)
	case "task_complete", "turn_complete":
		th.state, th.detail, th.stateAt, th.asking = model.StateDone, "", ts, ""
		if msg := errorText(e.Error); msg != "" {
			th.state, th.detail = model.StateError, msg
		}
		th.message(e.LastAgent)
	case "turn_aborted":
		th.state, th.detail, th.stateAt, th.asking = model.StateIdle, "已中断", ts, ""
	case "user_message":
		th.userMessage(e.Message)
	case "thread_name_updated":
		th.name = model.Truncate(e.ThreadName, providers.TitleLen)
	case "item_completed":
		// Paginated rollouts (Codex 0.147+) record messages only as items.
		if e.Item == nil {
			break
		}
		var text strings.Builder
		for _, c := range e.Item.Content {
			text.WriteString(c.Text)
		}
		switch e.Item.Type {
		case "UserMessage":
			th.userMessage(text.String())
		case "AgentMessage":
			th.message(text.String())
		}
	case "token_count":
		r.rateLimits(ts, e.RateLimits)
		if e.Info != nil && e.Info.Total != nil {
			return r.tokens(th, ts, *e.Info.Total)
		}
	}
	return false
}

// userMessage takes a prompt: the first one names the thread until it
// has a name, the latest one is a subagent's task.
func (th *thread) userMessage(text string) {
	if t := model.Truncate(text, providers.TitleLen); t != "" {
		if th.prompt == "" {
			th.prompt = t
		}
		th.task = t
	}
}

// message takes the assistant's latest text of the turn.
func (th *thread) message(text string) {
	if msg := providers.MessageLine(text); msg != "" {
		th.turn.Message = msg
	}
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
	th.turn.Output += d.Output
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
