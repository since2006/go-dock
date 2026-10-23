package claude

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/providers"
	"github.com/since2006/go-dock/host/internal/providers/pricing"
)

func TestHookObservation(t *testing.T) {
	now := time.Unix(1792200000, 0)
	cases := []struct {
		body   string
		state  model.State
		detail string
		end    bool
		skip   bool
	}{
		{body: `{"hook_event_name":"UserPromptSubmit","session_id":"s","cwd":"/w/p","prompt":"secret"}`, state: model.StateRunning},
		{body: `{"hook_event_name":"PermissionRequest","session_id":"s","tool_name":"Bash","tool_input":{"command":"npm install\nnpm test"}}`, state: model.StateApproval, detail: "Bash: npm install"},
		{body: `{"hook_event_name":"PreToolUse","session_id":"s","tool_name":"Edit","tool_input":{"file_path":"/w/p/main.go"}}`, state: model.StateRunning, detail: "Edit: main.go"},
		// A running command shows the description Claude wrote for it; an
		// approval shows the command itself.
		{body: `{"hook_event_name":"PreToolUse","session_id":"s","tool_name":"Bash","tool_input":{"command":"grep -c go-dock settings.json","description":"Check hooks install"}}`, state: model.StateRunning, detail: "Check hooks install"},
		{body: `{"hook_event_name":"PermissionRequest","session_id":"s","tool_name":"Bash","tool_input":{"command":"rm -rf dist","description":"Clean build output"}}`, state: model.StateApproval, detail: "Bash: rm -rf dist"},
		{body: `{"hook_event_name":"Notification","session_id":"s","notification_type":"idle_prompt"}`, skip: true},
		{body: `{"hook_event_name":"Notification","session_id":"s","notification_type":"elicitation_dialog","message":"Pick one"}`, state: model.StateInput, detail: "Pick one"},
		{body: `{"hook_event_name":"Stop","session_id":"s"}`, state: model.StateDone},
		{body: `{"hook_event_name":"StopFailure","session_id":"s","error":"rate_limit"}`, state: model.StateError, detail: "rate_limit"},
		{body: `{"hook_event_name":"SessionEnd","session_id":"s","reason":"other"}`, end: true},
		{body: `{"hook_event_name":"FileChanged","session_id":"s"}`, skip: true},
	}
	for _, c := range cases {
		obs, err := hookObservations([]byte(c.body), now)
		if err != nil {
			t.Fatalf("%s: %v", c.body, err)
		}
		if (len(obs) == 0) != c.skip {
			t.Fatalf("%s: got %d observations", c.body, len(obs))
		}
		if c.skip {
			continue
		}
		o := obs[0]
		if o.State != c.state || o.Detail != c.detail || o.End != c.end || o.SessionID != "s" || o.AgentID != "" {
			t.Errorf("%s: got %+v", c.body, o)
		}
	}
	if _, err := hookObservations([]byte(`{"hook_event_name":"Stop"}`), now); err == nil {
		t.Error("missing session_id accepted")
	}
}

func TestSubagentHooks(t *testing.T) {
	now := time.Unix(1792200000, 0)
	one := func(body string) model.Observation {
		t.Helper()
		obs, err := hookObservations([]byte(body), now)
		if err != nil || len(obs) != 1 {
			t.Fatalf("%s: %v %+v", body, err, obs)
		}
		return obs[0]
	}

	// The parent starts an agent: its description waits for the agent.
	o := one(`{"hook_event_name":"PreToolUse","session_id":"s","tool_name":"Agent","tool_input":{"description":"查找 API 入口","subagent_type":"Explore","prompt":"long"}}`)
	if !o.AgentLaunch || o.AgentType != "Explore" || o.AgentDesc != "查找 API 入口" || o.AgentID != "" || o.State != model.StateRunning {
		t.Fatalf("launch = %+v", o)
	}

	o = one(`{"hook_event_name":"SubagentStart","session_id":"s","agent_id":"a1","agent_type":"Explore"}`)
	if o.AgentID != "a1" || o.AgentType != "Explore" || o.State != model.StateRunning {
		t.Fatalf("start = %+v", o)
	}

	// Tool events inside the agent go to the agent, never the session.
	o = one(`{"hook_event_name":"PreToolUse","session_id":"s","agent_id":"a1","agent_type":"Explore","tool_name":"Read","tool_input":{"file_path":"/w/p/server.go"}}`)
	if o.AgentID != "a1" || o.Detail != "Read: server.go" || o.ToolDone != "" {
		t.Fatalf("agent tool = %+v", o)
	}
	o = one(`{"hook_event_name":"PermissionRequest","session_id":"s","agent_id":"a1","tool_name":"Bash","tool_input":{"command":"make"}}`)
	if o.AgentID != "a1" || o.State != model.StateApproval {
		t.Fatalf("agent permission = %+v", o)
	}
	o = one(`{"hook_event_name":"SubagentStop","session_id":"s","agent_id":"a1","agent_type":"Explore","last_assistant_message":"done"}`)
	if o.AgentID != "a1" || o.State != model.StateDone {
		t.Fatalf("stop = %+v", o)
	}

	// The Agent call's response maps the id to its description.
	obs, _ := hookObservations([]byte(`{"hook_event_name":"PostToolUse","session_id":"s","tool_name":"Agent","tool_input":{"description":"查找 API 入口","subagent_type":"Explore"},"tool_response":{"status":"async_launched","agentId":"a1"}}`), now)
	if len(obs) != 2 || obs[0].ToolDone != "Agent" || obs[1].AgentID != "a1" || obs[1].AgentDesc != "查找 API 入口" || obs[1].State != "" {
		t.Fatalf("post agent = %+v", obs)
	}

	// The main session's own tools are tallied; a new prompt resets.
	o = one(`{"hook_event_name":"PostToolUse","session_id":"s","tool_name":"Read","tool_input":{"file_path":"a.go"}}`)
	if o.ToolDone != "Read" || o.AgentID != "" {
		t.Fatalf("tally = %+v", o)
	}
	if o = one(`{"hook_event_name":"UserPromptSubmit","session_id":"s","prompt":"x"}`); !o.NewTurn {
		t.Fatal("prompt does not start a new turn")
	}
}

type fakeSink struct {
	obs   []model.Observation
	usage map[model.Provider]model.Usage
}

func (f *fakeSink) Observe(o model.Observation) { f.obs = append(f.obs, o) }
func (f *fakeSink) UpdateUsage(p model.Provider, fn func(*model.Usage)) {
	if f.usage == nil {
		f.usage = map[model.Provider]model.Usage{}
	}
	u := f.usage[p]
	fn(&u)
	f.usage[p] = u
}

func TestStatusline(t *testing.T) {
	sink := &fakeSink{}
	p := &Provider{sink: sink, titles: map[string]string{}}
	body := `{"session_id":"a","cost":{"total_api_duration_ms":1000},"rate_limits":{"five_hour":{"used_percentage":62.5,"resets_at":1792210000},"seven_day":{"used_percentage":31,"resets_at":1792600000}}}`
	if err := p.HandleStatusline([]byte(body)); err != nil {
		t.Fatal(err)
	}
	u := sink.usage[P]
	if u.FiveHour.UsedPct != 62.5 || u.FiveHour.ResetsAt.Unix() != 1792210000 || u.SevenDay.UsedPct != 31 || u.LimitsSource != "statusline" {
		t.Fatalf("usage = %+v", u)
	}
	// A later payload without seven_day keeps the old window.
	p.HandleStatusline([]byte(`{"session_id":"a","cost":{"total_api_duration_ms":2000},"rate_limits":{"five_hour":{"used_percentage":70,"resets_at":1792210000}}}`))
	if u := sink.usage[P]; u.FiveHour.UsedPct != 70 || u.SevenDay == nil {
		t.Fatalf("usage after partial = %+v", u)
	}
	// An idle session repeats the limits of its last reply: a lower number
	// in the same window and an earlier window change nothing, not even
	// when the limits were updated.
	updated := sink.usage[P].LimitsUpdated
	p.HandleStatusline([]byte(`{"session_id":"b","cost":{"total_api_duration_ms":500},"rate_limits":{"five_hour":{"used_percentage":40,"resets_at":1792210000},"seven_day":{"used_percentage":90,"resets_at":1792000000}}}`))
	if u := sink.usage[P]; u.FiveHour.UsedPct != 70 || u.SevenDay.UsedPct != 31 || !u.LimitsUpdated.Equal(updated) {
		t.Fatalf("usage after a stale session = %+v", u)
	}
	// A new window starts low.
	p.HandleStatusline([]byte(`{"session_id":"a","cost":{"total_api_duration_ms":3000},"rate_limits":{"five_hour":{"used_percentage":3,"resets_at":1792228000}}}`))
	if u := sink.usage[P]; u.FiveHour.UsedPct != 3 || u.FiveHour.ResetsAt.Unix() != 1792228000 || u.SevenDay.UsedPct != 31 {
		t.Fatalf("usage after a new window = %+v", u)
	}
	// API-key sessions have no rate_limits.
	if err := p.HandleStatusline([]byte(`{"session_id":"s"}`)); err != nil {
		t.Fatal(err)
	}
}

// After the user resets their usage, the same window (same resets_at)
// reports a lower number. Only a session that got a reply since its last
// report brings it; idle ones keep repeating the old, higher one.
func TestStatuslineReset(t *testing.T) {
	sink := &fakeSink{}
	p := &Provider{sink: sink, titles: map[string]string{}}
	report := func(session string, apiMs int, five, seven float64) {
		t.Helper()
		body := fmt.Sprintf(`{"session_id":%q,"cost":{"total_api_duration_ms":%d},"rate_limits":{"five_hour":{"used_percentage":%v,"resets_at":1792210000},"seven_day":{"used_percentage":%v,"resets_at":1792600000}}}`,
			session, apiMs, five, seven)
		if err := p.HandleStatusline([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	want := func(step string, five, seven float64) {
		t.Helper()
		if u := sink.usage[P]; u.FiveHour.UsedPct != five || u.SevenDay.UsedPct != seven {
			t.Fatalf("%s: 5h %v 7d %v, want %v %v", step, u.FiveHour.UsedPct, u.SevenDay.UsedPct, five, seven)
		}
	}

	// go-dock just started: nothing tells which session is current, so
	// the higher number wins.
	report("a", 1000, 2, 1)
	report("b", 500, 15, 10)
	want("first reports", 15, 10)

	// a gets a reply: it is current, though lower.
	report("a", 1200, 2, 1)
	want("a replied", 2, 1)
	report("b", 500, 15, 10)
	want("b repeats", 2, 1)

	// b's 5h window ran out and Claude Code dropped it: its 7d window is
	// still the old one.
	p.HandleStatusline([]byte(`{"session_id":"b","cost":{"total_api_duration_ms":500},"rate_limits":{"seven_day":{"used_percentage":10,"resets_at":1792600000}}}`))
	want("b dropped a window", 2, 1)

	// Without the API time, a changed window is the sign of a reply.
	p.HandleStatusline([]byte(`{"session_id":"c","rate_limits":{"five_hour":{"used_percentage":20,"resets_at":1792210000}}}`))
	want("c first", 2, 1)
	p.HandleStatusline([]byte(`{"session_id":"c","rate_limits":{"five_hour":{"used_percentage":4,"resets_at":1792210000}}}`))
	want("c changed", 4, 1)

	// A session that started after go-dock: its first limits follow a
	// report without any.
	p.HandleStatusline([]byte(`{"session_id":"d","cost":{"total_api_duration_ms":0}}`))
	report("d", 300, 0.5, 0.2)
	want("new session", 0.5, 0.2)
}

func TestLimitTrackerForgets(t *testing.T) {
	var lt limitTracker
	w := &model.Window{UsedPct: 5, ResetsAt: time.Unix(1792210000, 0)}
	at := time.Unix(1792200000, 0)
	lt.report("a", limitReport{apiMs: 100, five: w, at: at})
	if f, _ := lt.report("a", limitReport{apiMs: 200, five: w, at: at.Add(time.Hour)}); !f {
		t.Fatal("reply not fresh")
	}
	if f, _ := lt.report("a", limitReport{apiMs: 300, five: w, at: at.Add(time.Hour + limitForget + time.Second)}); f {
		t.Fatal("a forgotten session's report counted as fresh")
	}
}

func writeSession(t *testing.T, dir string, pid int, f map[string]any) {
	t.Helper()
	f["pid"] = pid
	b, _ := json.Marshal(f)
	if err := os.WriteFile(filepath.Join(dir, itoa(pid)+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestSessionsWatcher(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "1.abc.key"), []byte("secret"), 0o600)
	w := newSessionsWatcher(dir)
	alive := map[int]bool{1: true, 2: true, 3: false}
	w.alive = func(pid int) bool { return alive[pid] }
	now := time.UnixMilli(1792201900000)

	writeSession(t, dir, 1, map[string]any{"sessionId": "a", "cwd": "/w/a", "status": "busy", "startedAt": 1792201000000, "statusUpdatedAt": 1792201800000})
	writeSession(t, dir, 2, map[string]any{"sessionId": "b", "cwd": "/w/b", "status": "idle", "startedAt": 1792201000000, "statusUpdatedAt": 1792201001000})
	writeSession(t, dir, 3, map[string]any{"sessionId": "dead", "status": "busy"})

	got := map[string]model.Observation{}
	for _, o := range w.poll(now) {
		got[o.SessionID] = o
	}
	if len(got) != 2 || got["a"].State != model.StateRunning || got["b"].State != model.StateIdle {
		t.Fatalf("first poll = %+v", got)
	}
	if !got["a"].At.Equal(time.UnixMilli(1792201800000)) {
		t.Fatalf("At not from statusUpdatedAt: %v", got["a"].At)
	}
	if len(w.poll(now)) != 0 {
		t.Fatal("unchanged files produced observations")
	}

	writeSession(t, dir, 1, map[string]any{"sessionId": "a", "status": "waiting", "waitingFor": "permission prompt", "statusUpdatedAt": 1792201850000})
	writeSession(t, dir, 2, map[string]any{"sessionId": "b", "status": "waiting", "waitingFor": "input needed", "statusUpdatedAt": 1792201850000})
	got = map[string]model.Observation{}
	for _, o := range w.poll(now) {
		got[o.SessionID] = o
	}
	if got["a"].State != model.StateApproval || got["a"].Detail != "" || got["b"].State != model.StateInput || got["b"].Detail != "需要输入" {
		t.Fatalf("waiting poll = %+v", got)
	}

	writeSession(t, dir, 1, map[string]any{"sessionId": "a", "status": "idle", "statusUpdatedAt": 1792201860000})
	if obs := w.poll(now); len(obs) != 1 || obs[0].State != model.StateDone {
		t.Fatalf("idle after waiting = %+v", obs)
	}

	os.Remove(filepath.Join(dir, "2.json"))
	alive[1] = false
	obs := w.poll(now)
	if len(obs) != 2 || !obs[0].End || !obs[1].End {
		t.Fatalf("end poll = %+v", obs)
	}
}

func TestTranscripts(t *testing.T) {
	tr := newTranscripts(filepath.Join("testdata", "projects"), pricing.New(nil))
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	res := tr.poll(true, now)
	if !res.changed || res.titles["s1"] != "Build the dock host" {
		t.Fatalf("poll = %+v", res)
	}
	today, week := tr.totals(now)
	if today.Input != 210 || today.Output != 70 || today.CacheRead != 100 || today.CacheCreate != 0 {
		t.Fatalf("today = %+v", today)
	}
	if math.Abs(today.CostUSD-0.00109) > 1e-9 || !today.HasCost {
		t.Fatalf("today cost = %v", today.CostUSD)
	}
	if week.Input != 1210 || week.CacheCreate != 2000 || math.Abs(week.CostUSD-0.02109) > 1e-9 {
		t.Fatalf("week = %+v", week)
	}
	if res := tr.poll(false, now); res.changed {
		t.Fatal("second poll changed without new data")
	}
}

func TestTranscriptsPreferRenamedTitle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "-work-demo")
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "s1.jsonl")
	write := func(lines ...string) {
		f, _ := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		f.WriteString(strings.Join(lines, "\n") + "\n")
		f.Close()
	}
	tr := newTranscripts(filepath.Dir(dir), pricing.New(nil))
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

	write(`{"type":"ai-title","aiTitle":"go-dock-v1-host-plan","sessionId":"s1"}`,
		`{"type":"custom-title","customTitle":"detail","sessionId":"s1"}`)
	if got := tr.poll(true, now).titles["s1"]; got != "detail" {
		t.Fatalf("title = %q, want the /rename one", got)
	}
	// Claude Code keeps writing generated titles after a rename.
	write(`{"type":"ai-title","aiTitle":"another generated title","sessionId":"s1"}`)
	if got := tr.poll(false, now).titles["s1"]; got != "detail" {
		t.Fatalf("title after new ai-title = %q", got)
	}
}

func TestTranscriptTurn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "-work-demo")
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "s1.jsonl")
	write := func(lines ...string) {
		f, _ := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		f.WriteString(strings.Join(lines, "\n") + "\n")
		f.Close()
	}
	tr := newTranscripts(filepath.Dir(dir), pricing.New(nil))
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	const reply = "先把两张截图的像素颜色取出来，对比 ShellRS 的配色和渲染。"

	write(`{"type":"user","sessionId":"s1","message":{"role":"user","content":"对比一下配色"}}`,
		`{"type":"assistant","sessionId":"s1","message":{"id":"m1","content":[{"type":"text","text":"`+reply+`"}],"usage":{"output_tokens":50}}}`,
		`{"type":"assistant","sessionId":"s1","message":{"id":"m1","content":[{"type":"tool_use","name":"Bash","input":{}}],"usage":{"output_tokens":120}}}`,
		`{"type":"user","sessionId":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
		`{"type":"assistant","sessionId":"s1","message":{"id":"m3","content":[{"type":"tool_use","name":"Read","input":{}}],"usage":{"output_tokens":30}}}`,
		`{"type":"assistant","sessionId":"s1","isSidechain":true,"message":{"id":"m2","content":[{"type":"text","text":"sidechain"}],"usage":{"output_tokens":999}}}`)
	if got := tr.poll(true, now).turns["s1"]; got != (model.Turn{Message: reply, Output: 150}) {
		t.Fatalf("turn = %+v", got)
	}
	// A local slash command is not a new turn; a prompt is.
	write(`{"type":"user","sessionId":"s1","message":{"role":"user","content":"<command-name>/rename</command-name>"}}`)
	if res := tr.poll(false, now); len(res.turns) != 0 {
		t.Fatalf("slash command changed the turn: %+v", res.turns)
	}
	write(`{"type":"user","sessionId":"s1","message":{"role":"user","content":[{"type":"text","text":"再看看字体"}]}}`)
	if got, ok := tr.poll(false, now).turns["s1"]; !ok || got != (model.Turn{}) {
		t.Fatalf("new prompt: turn = %+v (reported %v)", got, ok)
	}
}

func TestInstallRoundTrip(t *testing.T) {
	home := t.TempDir()
	orig, _ := os.ReadFile(filepath.Join("testdata", "settings.json"))
	os.WriteFile(settingsPath(home), orig, 0o644)
	tg := providers.Target{Exe: "/opt/go-dock/go-dock", Addr: "127.0.0.1:47321"}

	if st := HooksStatus(home, tg); st.Installed {
		t.Fatal("reported installed before install")
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := InstallHooks(home, tg); err != nil {
			t.Fatal(err)
		}
		if err := InstallStatusline(home, tg); err != nil {
			t.Fatal(err)
		}
	}
	if st := HooksStatus(home, tg); !st.Installed || st.Stale {
		t.Fatalf("hooks status = %+v", st)
	}
	if st := StatuslineStatus(home, tg); !st.Installed || st.Stale {
		t.Fatalf("statusline status = %+v", st)
	}
	f, _ := providers.ReadJSONFile(settingsPath(home))
	if n := len(f.Get("hooks.Stop").Array()); n != 2 {
		t.Fatalf("Stop groups = %d, want user's + ours", n)
	}
	if n := len(f.Get("hooks.PreToolUse").Array()); n != 1 {
		t.Fatalf("PreToolUse groups = %d", n)
	}
	if f.Get("hooks.SessionEnd.0.hooks.0.async").Exists() || !f.Get("hooks.Stop.1.hooks.0.async").Bool() {
		t.Fatal("async flags wrong")
	}
	if f.Get("model").String() != "opus" || f.Get("statusLine.refreshInterval").Int() != 5 {
		t.Fatal("unrelated settings lost")
	}
	chain, _ := ChainFromCommand(f.Get("statusLine.command").String())
	if chain != `bash -c 'node "$HOME/.claude/hud/index.js"'` {
		t.Fatalf("chain = %q", chain)
	}

	// A different executable shows as stale.
	if st := HooksStatus(home, providers.Target{Exe: "/new/go-dock", Addr: tg.Addr}); !st.Stale {
		t.Fatal("moved exe not reported stale")
	}

	if err := UninstallHooks(home); err != nil {
		t.Fatal(err)
	}
	if err := UninstallStatusline(home); err != nil {
		t.Fatal(err)
	}
	f, _ = providers.ReadJSONFile(settingsPath(home))
	var a, b any
	json.Unmarshal(orig, &a)
	json.Unmarshal(f.Data, &b)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("uninstall did not restore settings:\n%s\n%s", ja, jb)
	}
	baks, _ := filepath.Glob(settingsPath(home) + ".go-dock-*.bak")
	if len(baks) == 0 {
		t.Fatal("no backup written")
	}
}

// Machines without a status line: install creates one with nothing to wrap,
// uninstall removes it again.
func TestStatuslineWithoutExisting(t *testing.T) {
	tg := providers.Target{Exe: "/opt/go-dock/go-dock", Addr: "127.0.0.1:47321"}
	cases := map[string]string{
		"no settings file":            "",
		"settings without statusLine": `{"model":"opus","hooks":{"Stop":[{"hooks":[{"type":"command","command":"true"}]}]}}`,
	}
	for name, orig := range cases {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			if orig != "" {
				os.WriteFile(settingsPath(home), []byte(orig), 0o644)
			}
			if st := StatuslineStatus(home, tg); st.Installed || st.Note != "" {
				t.Fatalf("status before install = %+v", st)
			}
			if err := InstallStatusline(home, tg); err != nil {
				t.Fatal(err)
			}
			f, err := providers.ReadJSONFile(settingsPath(home))
			if err != nil {
				t.Fatal(err)
			}
			cmd := f.Get("statusLine.command").String()
			if f.Get("statusLine.type").String() != "command" || cmd != statuslineCommand(tg, "") {
				t.Fatalf("statusLine = %s", f.Get("statusLine").Raw)
			}
			if strings.Contains(cmd, "--chain") {
				t.Fatalf("nothing to wrap but command has --chain: %s", cmd)
			}
			if st := StatuslineStatus(home, tg); !st.Installed || st.Stale {
				t.Fatalf("status after install = %+v", st)
			}

			if err := UninstallStatusline(home); err != nil {
				t.Fatal(err)
			}
			f, _ = providers.ReadJSONFile(settingsPath(home))
			if f.Get("statusLine").Exists() {
				t.Fatalf("statusLine left behind: %s", f.Data)
			}
			want := orig
			if want == "" {
				want = "{}"
			}
			var a, b any
			json.Unmarshal([]byte(want), &a)
			json.Unmarshal(f.Data, &b)
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("after uninstall = %s, want %s", f.Data, want)
			}
		})
	}
}

func TestSubagentTranscripts(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "-work-demo", "s9", "subagents")
	os.MkdirAll(dir, 0o755)
	now := time.Now()
	ts := now.Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	jsonlPath := filepath.Join(dir, "agent-a7.jsonl")
	os.WriteFile(jsonlPath, []byte(`{"type":"user","agentId":"a7","message":{"role":"user","content":"go"}}
{"type":"assistant","timestamp":"`+ts+`","message":{"id":"m1","model":"claude-haiku-5-5","content":[{"type":"text","text":"x"},{"type":"tool_use","name":"Grep","input":{"pattern":"TODO"}}],"usage":{"input_tokens":1,"output_tokens":1}}}
`), 0o644)
	meta := filepath.Join(dir, "agent-a7.meta.json")
	os.WriteFile(meta, []byte(`{"agentType":"Explore","description":"找出所有 TODO"}`), 0o644)

	tr := newTranscripts(root, pricing.New(nil))
	live := false
	tr.live = func(id string) bool { return live && id == "s9" }

	if res := tr.poll(true, now); len(res.agents) != 0 {
		t.Fatalf("agents of a session that is not running: %+v", res.agents)
	}
	live = true
	res := tr.poll(true, now)
	if len(res.agents) != 1 {
		t.Fatalf("agents = %+v", res.agents)
	}
	o := res.agents[0]
	if o.SessionID != "s9" || o.AgentID != "a7" || o.AgentType != "Explore" || o.AgentDesc != "找出所有 TODO" ||
		o.State != model.StateRunning || o.Detail != "Grep: TODO" {
		t.Fatalf("agent = %+v", o)
	}
	if res := tr.poll(true, now); len(res.agents) != 0 {
		t.Fatalf("unchanged agent reported again: %+v", res.agents)
	}

	os.WriteFile(meta, []byte(`{"agentType":"Explore","description":"找出所有 TODO","finished":true}`), 0o644)
	res = tr.poll(true, now)
	if len(res.agents) != 1 || res.agents[0].State != model.StateDone {
		t.Fatalf("finished agent = %+v", res.agents)
	}
}

func TestHooksStaleWithoutNewEvents(t *testing.T) {
	home := t.TempDir()
	tg := providers.Target{Exe: "/opt/go-dock/go-dock", Addr: "127.0.0.1:47321"}
	if err := InstallHooks(home, tg); err != nil {
		t.Fatal(err)
	}
	// An install from before SubagentStart/SubagentStop existed.
	f, _ := providers.ReadJSONFile(settingsPath(home))
	f.Delete("hooks.SubagentStart")
	f.Delete("hooks.SubagentStop")
	f.Save()
	if st := HooksStatus(home, tg); !st.Installed || !st.Stale {
		t.Fatalf("old install not reported as needing an update: %+v", st)
	}
	InstallHooks(home, tg)
	if st := HooksStatus(home, tg); st.Stale {
		t.Fatalf("reinstall still stale: %+v", st)
	}
}

func TestHookQuestionsWaitForUser(t *testing.T) {
	now := time.Unix(1792200000, 0)
	for tool, detail := range map[string]string{"AskUserQuestion": "等待回答问题", "ExitPlanMode": "等待确认计划"} {
		obs, err := hookObservations([]byte(`{"hook_event_name":"PreToolUse","session_id":"s","tool_name":"`+tool+`","tool_input":{}}`), now)
		if err != nil || len(obs) != 1 || obs[0].State != model.StateInput || obs[0].Detail != detail {
			t.Fatalf("%s: %+v %v", tool, obs, err)
		}
		// The answer arrives as PostToolUse: back to running.
		obs, _ = hookObservations([]byte(`{"hook_event_name":"PostToolUse","session_id":"s","tool_name":"`+tool+`","tool_input":{}}`), now)
		if obs[0].State != model.StateRunning {
			t.Fatalf("%s after answer: %+v", tool, obs)
		}
	}
}
