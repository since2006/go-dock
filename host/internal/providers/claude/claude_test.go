package claude

import (
	"encoding/json"
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
		{body: `{"hook_event_name":"Notification","session_id":"s","notification_type":"idle_prompt"}`, skip: true},
		{body: `{"hook_event_name":"Notification","session_id":"s","notification_type":"elicitation_dialog","message":"Pick one"}`, state: model.StateInput, detail: "Pick one"},
		{body: `{"hook_event_name":"Stop","session_id":"s"}`, state: model.StateDone},
		{body: `{"hook_event_name":"StopFailure","session_id":"s","error":"rate_limit"}`, state: model.StateError, detail: "rate_limit"},
		{body: `{"hook_event_name":"SessionEnd","session_id":"s","reason":"other"}`, end: true},
		{body: `{"hook_event_name":"FileChanged","session_id":"s"}`, skip: true},
	}
	for _, c := range cases {
		o, ok, err := hookObservation([]byte(c.body), now)
		if err != nil {
			t.Fatalf("%s: %v", c.body, err)
		}
		if ok == c.skip {
			t.Fatalf("%s: ok=%v", c.body, ok)
		}
		if c.skip {
			continue
		}
		if o.State != c.state || o.Detail != c.detail || o.End != c.end || o.SessionID != "s" {
			t.Errorf("%s: got %+v", c.body, o)
		}
	}
	if _, _, err := hookObservation([]byte(`{"hook_event_name":"Stop"}`), now); err == nil {
		t.Error("missing session_id accepted")
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
	body := `{"session_id":"s","rate_limits":{"five_hour":{"used_percentage":62.5,"resets_at":1792210000},"seven_day":{"used_percentage":31,"resets_at":1792600000}}}`
	if err := p.HandleStatusline([]byte(body)); err != nil {
		t.Fatal(err)
	}
	u := sink.usage[P]
	if u.FiveHour.UsedPct != 62.5 || u.FiveHour.ResetsAt.Unix() != 1792210000 || u.SevenDay.UsedPct != 31 || u.LimitsSource != "statusline" {
		t.Fatalf("usage = %+v", u)
	}
	// A later payload without seven_day keeps the old window.
	p.HandleStatusline([]byte(`{"rate_limits":{"five_hour":{"used_percentage":70,"resets_at":1792210000}}}`))
	if u := sink.usage[P]; u.FiveHour.UsedPct != 70 || u.SevenDay == nil {
		t.Fatalf("usage after partial = %+v", u)
	}
	// API-key sessions have no rate_limits.
	if err := p.HandleStatusline([]byte(`{"session_id":"s"}`)); err != nil {
		t.Fatal(err)
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
