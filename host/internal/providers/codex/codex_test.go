package codex

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/providers"
)

var testNow = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

const (
	idLegacy    = "11111111-aaaa"
	idPaginated = "22222222-bbbb"
	idSubagent  = "33333333-cccc"
)

// newHome lays the fixtures out like a real $CODEX_HOME.
func newHome(t *testing.T, withLocks bool, locks ...string) string {
	t.Helper()
	home := t.TempDir()
	copyFixture(t, "legacy.jsonl", filepath.Join(home, "sessions", "2026", "10", "17", "rollout-2026-10-17T09-00-00-"+idLegacy+".jsonl"))
	copyFixture(t, "paginated.jsonl", filepath.Join(home, "sessions", "2026", "10", "16", "rollout-2026-10-16T22-59-00-"+idPaginated+".jsonl"))
	copyFixture(t, "subagent.jsonl", filepath.Join(home, "sessions", "2026", "10", "17", "rollout-2026-10-17T10-00-00-"+idSubagent+".jsonl"))
	if withLocks {
		os.MkdirAll(filepath.Join(home, "thread-writer-locks"), 0o755)
		for _, id := range locks {
			setLock(t, home, id, true)
		}
	}
	return home
}

func copyFixture(t *testing.T, name, dst string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(dst), 0o755)
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func setLock(t *testing.T, home, id string, on bool) {
	t.Helper()
	p := filepath.Join(home, "thread-writer-locks", id+".lock")
	if on {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	} else {
		os.Remove(p)
	}
}

// byID keys session observations by session id and agent observations by
// "agent:" + agent id.
func byID(obs []model.Observation) map[string]model.Observation {
	m := map[string]model.Observation{}
	for _, o := range obs {
		if o.AgentID != "" {
			m["agent:"+o.AgentID] = o
		} else {
			m[o.SessionID] = o
		}
	}
	return m
}

func TestRolloutParsing(t *testing.T) {
	home := newHome(t, true, idLegacy, idSubagent)
	r := newRollouts(home, time.UTC)
	res := r.poll(true, testNow)

	got := byID(res.obs)
	if len(got) != 2 {
		t.Fatalf("observations = %+v, want the locked legacy thread and its subagent", res.obs)
	}
	// The spawned subagent is an agent of the session it works for.
	a := got["agent:"+idSubagent]
	if a.SessionID != idLegacy || a.AgentType != "explorer" || a.AgentDesc != "列出所有 API 路由" ||
		a.State != model.StateRunning || !a.At.Equal(time.Date(2026, 10, 17, 2, 0, 1, 0, time.UTC)) {
		t.Fatalf("subagent observation = %+v", a)
	}
	o := got[idLegacy]
	if o.State != model.StateIdle || o.Detail != "已中断" || o.Title != "Fix the flaky test" ||
		o.Cwd != "/work/legacy" || o.Source != "rollout" || !o.At.Equal(time.Date(2026, 10, 17, 1, 6, 30, 0, time.UTC)) {
		t.Fatalf("legacy observation = %+v", o)
	}

	// Rate limits: newest reading per window; other buckets and the older
	// flat-field reading are ignored.
	l := res.limits
	if l == nil || l.five == nil || l.seven == nil {
		t.Fatalf("limits = %+v", l)
	}
	if l.five.UsedPct != 20 || l.five.WindowMinutes != 300 || !l.five.ResetsAt.Equal(time.Date(2026, 10, 17, 2, 5, 0, 0, time.UTC)) {
		t.Fatalf("five = %+v", l.five)
	}
	if l.seven.UsedPct != 31 || l.seven.WindowMinutes != 10080 || l.seven.ResetsAt.Unix() != 1792600000 {
		t.Fatalf("seven = %+v", l.seven)
	}
	if !l.at.Equal(time.Date(2026, 10, 17, 1, 5, 0, 0, time.UTC)) || l.source != "rollout" {
		t.Fatalf("limits at = %v source %s", l.at, l.source)
	}

	// Token deltas: legacy 600+400 input, 400+1600 cached, 100+200 output;
	// the hidden subagent still counts; the paginated thread is yesterday.
	if !res.tokensChanged {
		t.Fatal("tokens not reported changed")
	}
	today, week := r.totals(testNow)
	want := model.Tokens{Input: 1100, CacheRead: 2000, Output: 310}
	if today != want {
		t.Fatalf("today = %+v, want %+v", today, want)
	}
	wantWeek := model.Tokens{Input: 1600, CacheRead: 2000, Output: 360, CacheCreate: 100}
	if week != wantWeek {
		t.Fatalf("week = %+v, want %+v", week, wantWeek)
	}

	// Nothing new: no observations, no limit or token changes.
	res = r.poll(false, testNow)
	if len(res.obs) != 0 || res.limits != nil || res.tokensChanged {
		t.Fatalf("idle poll = %+v", res)
	}

	// Opening the paginated thread reports its finished turn.
	setLock(t, home, idPaginated, true)
	res = r.poll(false, testNow)
	got = byID(res.obs)
	if p := got[idPaginated]; len(got) != 1 || p.State != model.StateDone || p.Title != "Add a README" || p.Cwd != "/work/pag" {
		t.Fatalf("paginated = %+v", res.obs)
	}

	// Closing the subagent's thread ends the agent, not the session.
	setLock(t, home, idSubagent, false)
	res = r.poll(false, testNow)
	if len(res.obs) != 1 || !res.obs[0].End || res.obs[0].SessionID != idLegacy || res.obs[0].AgentID != idSubagent {
		t.Fatalf("subagent close = %+v", res.obs)
	}

	// Closing the legacy thread ends it; reopening reports the new turn.
	setLock(t, home, idLegacy, false)
	res = r.poll(false, testNow)
	if len(res.obs) != 1 || !res.obs[0].End || res.obs[0].SessionID != idLegacy {
		t.Fatalf("close = %+v", res.obs)
	}
	legacy := filepath.Join(home, "sessions", "2026", "10", "17", "rollout-2026-10-17T09-00-00-"+idLegacy+".jsonl")
	f, _ := os.OpenFile(legacy, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"timestamp":"2026-10-17T11:00:00.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t3"}}` + "\n")
	f.Close()
	if res = r.poll(false, testNow); len(res.obs) != 0 {
		t.Fatalf("closed thread emitted %+v", res.obs)
	}
	setLock(t, home, idLegacy, true)
	res = r.poll(false, testNow)
	if len(res.obs) != 1 || res.obs[0].State != model.StateRunning || res.obs[0].Detail != "" {
		t.Fatalf("reopen = %+v", res.obs)
	}
}

func TestRolloutWithoutLocks(t *testing.T) {
	home := newHome(t, false)
	old := filepath.Join(home, "sessions", "2026", "10", "16", "rollout-2026-10-16T22-59-00-"+idPaginated+".jsonl")
	past := time.Now().Add(-3 * time.Hour)
	os.Chtimes(old, past, past)

	r := newRollouts(home, time.UTC)
	got := byID(r.poll(true, time.Now()).obs)
	if len(got) != 2 || got[idLegacy].State != model.StateIdle || got["agent:"+idSubagent].SessionID != idLegacy {
		t.Fatalf("mtime mode = %+v", got)
	}
}

func TestNewFileFoundBetweenScans(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, "thread-writer-locks"), 0o755)
	setLock(t, home, idLegacy, true)
	r := newRollouts(home, time.UTC)
	if res := r.poll(true, testNow); len(res.obs) != 0 {
		t.Fatalf("empty home = %+v", res.obs)
	}
	copyFixture(t, "legacy.jsonl", filepath.Join(home, "sessions", "2026", "10", "17", "rollout-2026-10-17T09-00-00-"+idLegacy+".jsonl"))
	if res := r.poll(false, testNow); len(res.obs) != 1 {
		t.Fatalf("fast poll missed today's new file: %+v", res.obs)
	}
}

func TestTokenBaselineReset(t *testing.T) {
	r := newRollouts(t.TempDir(), time.UTC)
	th := &thread{}
	r.tokens(th, testNow, tokenUsage{Input: 100, Output: 10, Total: 110})
	r.tokens(th, testNow, tokenUsage{Input: 50, Output: 5, Total: 55}) // dropped: new baseline
	r.tokens(th, testNow, tokenUsage{Input: 80, Output: 8, Total: 88})
	today, _ := r.totals(testNow)
	if today.Input != 130 || today.Output != 13 {
		t.Fatalf("today = %+v", today)
	}
}

func TestParseRateLimitsFlat(t *testing.T) {
	five, seven := parseRateLimits(json.RawMessage(`{"primary_used_percent":5.5,"secondary_used_percent":9}`), testNow)
	if five == nil || five.UsedPct != 5.5 || five.WindowMinutes != 300 || !five.ResetsAt.IsZero() {
		t.Fatalf("five = %+v", five)
	}
	if seven == nil || seven.UsedPct != 9 || seven.WindowMinutes != 10080 {
		t.Fatalf("seven = %+v", seven)
	}
	if f, s := parseRateLimits(json.RawMessage(`null`), testNow); f != nil || s != nil {
		t.Fatal("null rate_limits parsed")
	}
}

func TestHookObservation(t *testing.T) {
	cases := []struct {
		body   string
		state  model.State
		detail string
		title  string
		end    bool
		skip   bool
	}{
		{body: `{"hook_event_name":"SessionStart","session_id":"s","source":"startup"}`, state: model.StateIdle},
		{body: `{"hook_event_name":"UserPromptSubmit","session_id":"s","prompt":"Refactor the parser\nplease"}`, state: model.StateRunning, title: "Refactor the parser"},
		{body: `{"hook_event_name":"PermissionRequest","session_id":"s","tool_name":"Bash","tool_input":{"command":"rm -rf build","description":"clean"}}`, state: model.StateApproval, detail: "Bash: rm -rf build"},
		{body: `{"hook_event_name":"PostToolUse","session_id":"s","tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch"}}`, state: model.StateRunning, detail: "apply_patch: *** Begin Patch"},
		{body: `{"hook_event_name":"Stop","session_id":"s","last_assistant_message":"ok"}`, state: model.StateDone},
		{body: `{"hook_event_name":"Interrupt","session_id":"s"}`, state: model.StateIdle, detail: "已中断"},
		{body: `{"hook_event_name":"SessionEnd","session_id":"s"}`, end: true},
		{body: `{"hook_event_name":"PreCompact","session_id":"s"}`, skip: true},
	}
	for _, c := range cases {
		o, ok, err := hookObservation([]byte(c.body), testNow)
		if err != nil {
			t.Fatalf("%s: %v", c.body, err)
		}
		if ok == c.skip {
			t.Fatalf("%s: ok=%v", c.body, ok)
		}
		if c.skip {
			continue
		}
		if o.Provider != P || o.State != c.state || o.Detail != c.detail || o.Title != c.title || o.End != c.end || o.Source != "hook" {
			t.Errorf("%s: got %+v", c.body, o)
		}
	}
	if _, _, err := hookObservation([]byte(`{"hook_event_name":"Stop"}`), testNow); err == nil {
		t.Error("missing session_id accepted")
	}
}

type fakeSink struct{ usage model.Usage }

func (f *fakeSink) Observe(model.Observation) {}
func (f *fakeSink) UpdateUsage(_ model.Provider, fn func(*model.Usage)) {
	fn(&f.usage)
}

func TestPublishLimitsKeepsNewer(t *testing.T) {
	sink := &fakeSink{}
	p := &Provider{sink: sink}
	app := limits{five: &model.Window{UsedPct: 42}, at: testNow, source: "app-server"}
	p.publishLimits(app)
	p.publishLimits(limits{five: &model.Window{UsedPct: 10}, at: testNow.Add(-time.Minute), source: "rollout"})
	if sink.usage.FiveHour.UsedPct != 42 || sink.usage.LimitsSource != "app-server" {
		t.Fatalf("older rollout reading clobbered app-server: %+v", sink.usage)
	}
	p.publishLimits(limits{five: &model.Window{UsedPct: 50}, at: testNow.Add(time.Minute), source: "rollout"})
	if sink.usage.FiveHour.UsedPct != 50 {
		t.Fatalf("newer rollout reading ignored: %+v", sink.usage)
	}
}

func TestReadRateLimits(t *testing.T) {
	server := strings.Join([]string{
		`{"method":"codex/event","params":{}}`,
		`{"id":0,"result":{"userAgent":"codex"}}`,
		`not json`,
		`{"method":"account/rateLimits/updated","params":{}}`,
		`{"id":1,"result":{"rateLimits":{"limitId":"codex_other","primary":{"usedPercent":99}},"rateLimitsByLimitId":{"codex":{"limitId":"codex","primary":{"usedPercent":42,"windowDurationMins":300,"resetsAt":1792210000},"secondary":{"usedPercent":7,"windowDurationMins":10080,"resetsAt":1792600000}}}}}`,
	}, "\n") + "\n"
	var sent bytes.Buffer
	l, err := readRateLimits(strings.NewReader(server), &sent, "1.2.3", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if l.five.UsedPct != 42 || l.five.ResetsAt.Unix() != 1792210000 || l.seven.UsedPct != 7 || l.seven.WindowMinutes != 10080 {
		t.Fatalf("limits = %+v %+v", l.five, l.seven)
	}
	if !l.at.Equal(testNow) || l.source != "app-server" {
		t.Fatalf("at/source = %v %s", l.at, l.source)
	}

	lines := strings.Split(strings.TrimSpace(sent.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("sent %d lines: %s", len(lines), sent.String())
	}
	var init map[string]any
	json.Unmarshal([]byte(lines[0]), &init)
	ci := init["params"].(map[string]any)["clientInfo"].(map[string]any)
	if init["method"] != "initialize" || init["id"].(float64) != 0 || ci["name"] != "go-dock" || ci["version"] != "1.2.3" {
		t.Fatalf("initialize = %s", lines[0])
	}
	if strings.Contains(sent.String(), "jsonrpc") {
		t.Fatal("jsonrpc field sent")
	}
	if lines[1] != `{"method":"initialized"}` || !strings.Contains(lines[2], `"method":"account/rateLimits/read"`) {
		t.Fatalf("sent = %s", sent.String())
	}

	// Fallback to rateLimits when the codex bucket is absent.
	l, err = readRateLimits(strings.NewReader(`{"id":0,"result":{}}`+"\n"+`{"id":1,"result":{"rateLimits":{"primary":{"usedPercent":3}}}}`+"\n"), &bytes.Buffer{}, "dev", testNow)
	if err != nil || l.five.UsedPct != 3 || l.five.WindowMinutes != 300 || l.seven != nil {
		t.Fatalf("fallback = %+v, %v", l, err)
	}

	// Errors and early exit.
	if _, err := readRateLimits(strings.NewReader(`{"id":0,"error":{"code":-32600,"message":"bad"}}`+"\n"), &bytes.Buffer{}, "dev", testNow); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("error response: %v", err)
	}
	if _, err := readRateLimits(strings.NewReader(`{"id":0,"result":{}}`+"\n"), &bytes.Buffer{}, "dev", testNow); err == nil {
		t.Fatal("early exit not reported")
	}
}

func TestInstallRoundTrip(t *testing.T) {
	home := t.TempDir()
	orig, _ := os.ReadFile(filepath.Join("testdata", "hooks.json"))
	os.WriteFile(hooksPath(home), orig, 0o644)
	tg := providers.Target{Exe: "/opt/go-dock/go-dock", Addr: "127.0.0.1:47321"}

	if st := HooksStatus(home, tg); st.Installed || st.Note == "" {
		t.Fatalf("status before install = %+v", st)
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := InstallHooks(home, tg); err != nil {
			t.Fatal(err)
		}
	}
	st := HooksStatus(home, tg)
	if !st.Installed || st.Stale || !strings.Contains(st.Note, "/hooks") {
		t.Fatalf("status = %+v", st)
	}

	f, _ := providers.ReadJSONFile(hooksPath(home))
	for k := range f.Get("@this").Map() {
		if k != "description" && k != "hooks" {
			t.Fatalf("unexpected top-level key %q", k)
		}
	}
	stop := f.Get("hooks.Stop").Array()
	if len(stop) != 2 || stop[0].Get("hooks.0.command").String() != "say done" {
		t.Fatalf("user hook not kept first: %s", f.Get("hooks.Stop").Raw)
	}
	if cmd, want := stop[1].Get("hooks.0.command").String(), providers.ShellCommand(tg.Exe, "hook", "codex", "--addr", tg.Addr); cmd != want {
		t.Fatalf("our command = %q", cmd)
	}
	if !stop[1].Get("hooks.0.async").Bool() || f.Get("hooks.SessionEnd.0.hooks.0.async").Exists() {
		t.Fatal("async flags wrong")
	}
	if n := len(f.Get("hooks").Map()); n != len(hookEvents) {
		t.Fatalf("events = %d", n)
	}

	if st := HooksStatus(home, providers.Target{Exe: "/new/go-dock", Addr: tg.Addr}); !st.Stale {
		t.Fatal("moved exe not reported stale")
	}
	// Reinstalling at a new path replaces in place: the user's hook stays at
	// index 0, ours at index 1, so Codex trust keys for others don't shift.
	if err := InstallHooks(home, providers.Target{Exe: "/new/go-dock", Addr: tg.Addr}); err != nil {
		t.Fatal(err)
	}
	f, _ = providers.ReadJSONFile(hooksPath(home))
	if stop := f.Get("hooks.Stop").Array(); len(stop) != 2 || firstField(stop[1].Get("hooks.0.command").String()) != "/new/go-dock" {
		t.Fatalf("reinstall = %s", f.Get("hooks.Stop").Raw)
	}

	if err := UninstallHooks(home); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(hooksPath(home))
	if err != nil {
		t.Fatal(err)
	}
	var a, c any
	json.Unmarshal(orig, &a)
	json.Unmarshal(b, &c)
	ja, _ := json.Marshal(a)
	jc, _ := json.Marshal(c)
	if string(ja) != string(jc) {
		t.Fatalf("uninstall did not restore:\n%s\n%s", ja, jc)
	}
}

func TestInstallFreshAndMissingHome(t *testing.T) {
	home := t.TempDir()
	tg := providers.Target{Exe: "/opt/go-dock/go-dock", Addr: "127.0.0.1:47321"}
	if err := InstallHooks(home, tg); err != nil {
		t.Fatal(err)
	}
	if err := UninstallHooks(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hooksPath(home)); !os.IsNotExist(err) {
		t.Fatalf("empty hooks.json not removed: %v", err)
	}
	if err := UninstallHooks(home); err != nil {
		t.Fatalf("uninstall without file: %v", err)
	}

	missing := filepath.Join(home, "nope")
	if err := InstallHooks(missing, tg); err == nil || !strings.Contains(err.Error(), "未找到 Codex 目录") {
		t.Fatalf("missing home: %v", err)
	}
	if st := HooksStatus(missing, tg); st.Installed || !strings.Contains(st.Note, "未找到 Codex 目录") {
		t.Fatalf("missing home status = %+v", st)
	}
}

func TestFirstField(t *testing.T) {
	cases := map[string]string{
		`/opt/go-dock/go-dock hook codex`:              "/opt/go-dock/go-dock",
		`'/my apps/go-dock' hook codex`:                "/my apps/go-dock",
		`& 'C:/Program Files/go-dock.exe' hook codex`:  "C:/Program Files/go-dock.exe",
		`"C:/tools/go-dock.exe" hook codex --addr x:1`: "C:/tools/go-dock.exe",
	}
	for in, want := range cases {
		if got := firstField(in); got != want {
			t.Errorf("firstField(%q) = %q, want %q", in, got, want)
		}
		if !isOurHook(map[string]any{"command": in}) {
			t.Errorf("isOurHook(%q) = false", in)
		}
	}
	if isOurHook(map[string]any{"command": "say done"}) || isOurHook(map[string]any{"command": "/bin/go-dock hook claude"}) {
		t.Error("foreign hook matched")
	}
}

func TestSubagentHooks(t *testing.T) {
	now := time.Unix(1792200000, 0)
	o, ok, err := hookObservation([]byte(`{"hook_event_name":"SubagentStart","session_id":"root","agent_id":"t2","agent_type":"explorer","turn_id":"x"}`), now)
	if err != nil || !ok || o.SessionID != "root" || o.AgentID != "t2" || o.AgentType != "explorer" || o.State != model.StateRunning {
		t.Fatalf("start = %+v %v %v", o, ok, err)
	}
	o, _, _ = hookObservation([]byte(`{"hook_event_name":"PreToolUse","session_id":"root","agent_id":"t2","agent_type":"explorer","tool_name":"Bash","tool_input":{"command":"rg TODO"}}`), now)
	if o.AgentID != "t2" || o.Detail != "Bash: rg TODO" || o.ToolDone != "" {
		t.Fatalf("agent tool = %+v", o)
	}
	o, _, _ = hookObservation([]byte(`{"hook_event_name":"SubagentStop","session_id":"root","agent_id":"t2","agent_type":"explorer"}`), now)
	if o.AgentID != "t2" || o.State != model.StateDone {
		t.Fatalf("stop = %+v", o)
	}
	o, _, _ = hookObservation([]byte(`{"hook_event_name":"PostToolUse","session_id":"root","tool_name":"Bash","tool_input":{"command":"ls"}}`), now)
	if o.AgentID != "" || o.ToolDone != "Bash" {
		t.Fatalf("main tool = %+v", o)
	}
	o, _, _ = hookObservation([]byte(`{"hook_event_name":"UserPromptSubmit","session_id":"root","prompt":"hi"}`), now)
	if !o.NewTurn || o.AgentID != "" {
		t.Fatalf("prompt = %+v", o)
	}
}

func TestHooksStaleWithoutSubagentEvents(t *testing.T) {
	home := t.TempDir()
	tg := providers.Target{Exe: "/opt/go-dock/go-dock", Addr: "127.0.0.1:47321"}
	if err := InstallHooks(home, tg); err != nil {
		t.Fatal(err)
	}
	f, _ := providers.ReadJSONFile(hooksPath(home))
	f.Delete("hooks.SubagentStart")
	f.Delete("hooks.SubagentStop")
	f.Save()
	if st := HooksStatus(home, tg); !st.Installed || !st.Stale {
		t.Fatalf("old install not reported as needing an update: %+v", st)
	}
}
