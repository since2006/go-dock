package device

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
)

var t0 = time.Unix(1792200000, 0)

func sampleSnapshot() model.Snapshot {
	return model.Snapshot{
		Now: t0,
		Sessions: []model.Session{
			{ID: "claude:a", Provider: model.ProviderClaude, Project: "go-dock", State: model.StateApproval, Detail: "Bash: npm install", Title: "build host", Since: t0.Add(-42 * time.Second)},
			{ID: "codex:b", Provider: model.ProviderCodex, Project: "api-svc", State: model.StateRunning, Title: "重构登录模块", Since: t0.Add(-192 * time.Second)},
			{ID: "claude:c", Provider: model.ProviderClaude, Project: "blog", State: model.StateDone, Since: t0.Add(-2 * time.Minute)},
			{ID: "claude:d", Provider: model.ProviderClaude, Project: "old", State: model.StateIdle, Since: t0.Add(-time.Hour)},
		},
		Usage: map[model.Provider]model.Usage{
			model.ProviderClaude: {
				FiveHour:      &model.Window{UsedPct: 62, ResetsAt: t0.Add(2 * time.Hour), WindowMinutes: 300},
				LimitsUpdated: t0.Add(-time.Minute),
				Today:         model.Tokens{Input: 100, Output: 50, CostUSD: 1.5, HasCost: true},
			},
			model.ProviderCodex: {
				SevenDay: &model.Window{UsedPct: 12, WindowMinutes: 10080},
				Today:    model.Tokens{Input: 7},
			},
		},
		Counts: map[model.State]int{model.StateApproval: 1, model.StateRunning: 1, model.StateDone: 1, model.StateIdle: 1},
	}
}

func TestBuildFrame(t *testing.T) {
	f := BuildFrame(sampleSnapshot(), Options{MaxSessions: 12}, 7)
	if f.T != "snapshot" || f.Seq != 7 || f.Now != t0.Unix() {
		t.Fatalf("header = %+v", f)
	}
	if len(f.Sessions) != 3 || f.Total != 3 {
		t.Fatalf("idle not filtered: %d sessions, total %d", len(f.Sessions), f.Total)
	}
	s := f.Sessions[0]
	if s.P != "claude" || s.State != "approval" || s.Label != "需授权" || s.Text != "Bash: npm install" || s.Since != t0.Unix()-42 {
		t.Fatalf("session 0 = %+v", s)
	}
	if f.Sessions[1].Text != "重构登录模块" { // falls back to title
		t.Fatalf("session 1 text = %q", f.Sessions[1].Text)
	}
	c := f.Usage.Claude
	if c.FiveHour.Pct != 62 || c.FiveHour.ResetsAt != t0.Unix()+7200 || c.SevenDay != nil || c.TodayTokens != 150 || c.TodayCost == nil || *c.TodayCost != 1.5 {
		t.Fatalf("claude usage = %+v", c)
	}
	x := f.Usage.Codex
	if x.TodayCost != nil || x.SevenDay.ResetsAt != 0 || x.UpdatedAt != 0 || x.TodayTokens != 7 {
		t.Fatalf("codex usage = %+v", x)
	}
	if f.Counts.Approval != 1 || f.Counts.Idle != 1 {
		t.Fatalf("counts = %+v", f.Counts)
	}

	line, err := EncodeSnapshot(f)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(line, []byte("\n")) || bytes.Count(line, []byte("\n")) != 1 {
		t.Fatal("not a single line")
	}
	var generic map[string]any
	json.Unmarshal(line, &generic)
	if u := generic["usage"].(map[string]any)["codex"].(map[string]any); u["five_hour"] != nil {
		t.Fatalf("missing window should be null: %v", u["five_hour"])
	}
	if _, ok := generic["usage"].(map[string]any)["codex"].(map[string]any)["today_cost"]; ok {
		t.Fatal("today_cost present without cost")
	}

	f = BuildFrame(sampleSnapshot(), Options{MaxSessions: 2, ShowIdle: true}, 1)
	if len(f.Sessions) != 2 || f.Total != 4 {
		t.Fatalf("cap: %d sessions, total %d", len(f.Sessions), f.Total)
	}
}

func TestEncodeCap(t *testing.T) {
	s := sampleSnapshot()
	s.Sessions = nil
	for i := range 32 {
		s.Sessions = append(s.Sessions, model.Session{
			ID:       fmt.Sprintf("claude:%08d-aaaa-bbbb-cccc-dddddddddddd", i),
			Provider: model.ProviderClaude,
			Project:  strings.Repeat("项", 40),
			State:    model.StateRunning,
			Detail:   strings.Repeat("长", 80) + "<&>",
			Since:    t0,
		})
	}
	f := BuildFrame(s, Options{MaxSessions: 32}, 1)
	line, err := EncodeSnapshot(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(line) > MaxLine {
		t.Fatalf("line is %d bytes", len(line))
	}
	var got Snapshot
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) == 0 || len(got.Sessions) >= 32 || got.Total != 32 {
		t.Fatalf("trimmed to %d sessions, total %d", len(got.Sessions), got.Total)
	}
	if got.Sessions[0].ID != f.Sessions[0].ID {
		t.Fatal("trimmed from the head")
	}
	if n := len([]rune(got.Sessions[0].Project)); n != projectLen {
		t.Fatalf("project not truncated: %d runes", n)
	}
	if bytes.Contains(line, []byte(`<`)) {
		t.Fatal("HTML escaping enabled")
	}
}

func TestDecodeDeviceLine(t *testing.T) {
	m, err := DecodeDeviceLine([]byte(`{"t":"hello","proto":1,"fw":"0.1.0","w":600,"h":450,"max_line":4096}` + "\r\n"))
	if err != nil || m.T != "hello" || m.FW != "0.1.0" || m.W != 600 || m.MaxLine != 4096 {
		t.Fatalf("hello = %+v, %v", m, err)
	}
	if m, err := DecodeDeviceLine([]byte(`{"t":"ack","seq":42,"extra":true}`)); err != nil || m.Seq != 42 {
		t.Fatalf("ack = %+v, %v", m, err)
	}
	for _, bad := range []string{"", "ESP-ROM:esp32s3-20210327", "rst:0x1 (POWERON),boot:0x8", `{"seq":1}`, `{"t":`, `[1,2]`} {
		if _, err := DecodeDeviceLine([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// fakeSource is a SnapshotSource whose change channel the test drives.
type fakeSource struct {
	mu   sync.Mutex
	snap model.Snapshot
	ch   chan struct{}
}

func (f *fakeSource) Snapshot() model.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeSource) Subscribe() (<-chan struct{}, func()) { return f.ch, func() {} }

func (f *fakeSource) set(s model.Snapshot) {
	f.mu.Lock()
	f.snap = s
	f.mu.Unlock()
	f.ch <- struct{}{}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fakeDevice is the device end of a net.Pipe.
type fakeDevice struct {
	conn  net.Conn
	lines chan map[string]any
}

func newFakeDevice(conn net.Conn) *fakeDevice {
	d := &fakeDevice{conn: conn, lines: make(chan map[string]any, 64)}
	go func() {
		br := bufio.NewReader(conn)
		for {
			line, err := br.ReadBytes('\n')
			if err != nil {
				close(d.lines)
				return
			}
			var m map[string]any
			if json.Unmarshal(line, &m) == nil {
				d.lines <- m
			}
		}
	}()
	return d
}

func (d *fakeDevice) send(t *testing.T, s string) {
	t.Helper()
	if _, err := io.WriteString(d.conn, s+"\n"); err != nil {
		t.Fatal(err)
	}
}

// next returns the next message of type typ, skipping others.
func (d *fakeDevice) next(t *testing.T, typ string) map[string]any {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case m, ok := <-d.lines:
			if !ok {
				t.Fatalf("link closed waiting for %s", typ)
			}
			if m["t"] == typ {
				return m
			}
		case <-timeout:
			t.Fatalf("timeout waiting for %s", typ)
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func newTestManager(t *testing.T, src SnapshotSource, logs io.Writer) (*Manager, chan *fakeDevice) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := NewManager(src, Options{Enabled: true, Port: "auto", MaxSessions: 12}, "0.1.0", log)
	devices := make(chan *fakeDevice, 4)
	m.list = func() ([]PortInfo, error) {
		return []PortInfo{
			{Name: "/dev/cu.Bluetooth"},
			{Name: "/dev/cu.usbmodem9", VID: "303A", PID: "0002", Candidate: true},
			{Name: "/dev/cu.usbmodem1", VID: "303A", PID: "1001", Candidate: true},
		}, nil
	}
	m.open = func(name string) (io.ReadWriteCloser, error) {
		if name != "/dev/cu.usbmodem1" {
			return nil, fmt.Errorf("unexpected port %s", name)
		}
		host, dev := net.Pipe()
		devices <- newFakeDevice(dev)
		return host, nil
	}
	m.heartbeat, m.helloEvery, m.rescan, m.debounce = time.Hour, time.Hour, 20*time.Millisecond, 10*time.Millisecond
	return m, devices
}

func TestManagerLink(t *testing.T) {
	src := &fakeSource{snap: sampleSnapshot(), ch: make(chan struct{}, 1)}
	logs := &syncBuffer{}
	m, devices := newTestManager(t, src, logs)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	dev := <-devices
	if h := dev.next(t, "hello"); h["proto"] != 1.0 || h["host"] != "go-dock" || h["ver"] != "0.1.0" {
		t.Fatalf("hello = %v", h)
	}
	first := dev.next(t, "snapshot")
	if st := m.Status(); st.State != StateConnected || st.Port != "/dev/cu.usbmodem1" {
		t.Fatalf("status = %+v", st)
	}

	dev.send(t, "ESP-ROM:esp32s3-20210327 boot noise")
	dev.send(t, `{"t":"hello","proto":1,"fw":"0.0.1","w":600,"h":450,"max_line":4096}`)
	waitFor(t, "ready", func() bool { return m.Status().State == StateReady })
	if st := m.Status(); st.Firmware != "0.0.1" || st.Width != 600 || st.LastHeard.IsZero() {
		t.Fatalf("ready status = %+v", st)
	}
	onReady := dev.next(t, "snapshot")
	if onReady["seq"].(float64) <= first["seq"].(float64) {
		t.Fatal("seq did not increase")
	}

	s := sampleSnapshot()
	s.Sessions = s.Sessions[:1]
	src.set(s)
	changed := dev.next(t, "snapshot")
	if n := len(changed["sessions"].([]any)); n != 1 {
		t.Fatalf("change not pushed, %d sessions", n)
	}

	dev.send(t, `{"t":"ack","seq":3}`)
	dev.send(t, `{"t":"log","level":"warn","msg":"low battery"}`)
	waitFor(t, "device log", func() bool { return strings.Contains(logs.String(), "low battery") })
	if !strings.Contains(logs.String(), "src=device") || !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("device log not forwarded properly:\n%s", logs.String())
	}

	// Unplug: the manager reconnects and handshakes again.
	dev.conn.Close()
	dev2 := <-devices
	dev2.next(t, "hello")
	waitFor(t, "connected again", func() bool { return m.Status().State == StateConnected })

	// Disabling closes the link.
	m.SetOptions(Options{Enabled: false, Port: "auto"})
	waitFor(t, "disabled", func() bool { return m.Status().State == StateDisabled })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestManagerIncompatibleDevice(t *testing.T) {
	src := &fakeSource{snap: sampleSnapshot(), ch: make(chan struct{}, 1)}
	m, devices := newTestManager(t, src, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	dev := <-devices
	dev.next(t, "snapshot")
	dev.send(t, `{"t":"hello","proto":2,"fw":"9.0.0"}`)
	waitFor(t, "error", func() bool { return strings.Contains(m.Status().Error, "不兼容") })

	src.set(sampleSnapshot())
	select {
	case msg := <-dev.lines:
		t.Fatalf("sent %v to an incompatible device", msg)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestManagerNoDevice(t *testing.T) {
	src := &fakeSource{snap: sampleSnapshot(), ch: make(chan struct{}, 1)}
	m, _ := newTestManager(t, src, io.Discard)
	m.list = func() ([]PortInfo, error) { return []PortInfo{{Name: "/dev/cu.Bluetooth"}}, nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	waitFor(t, "not found", func() bool {
		st := m.Status()
		return st.State == StateSearching && strings.Contains(st.Error, "未找到设备")
	})
}

func TestPickPort(t *testing.T) {
	ports := []PortInfo{
		{Name: "COM9", VID: "303A", PID: "4002", Candidate: true},
		{Name: "COM4", VID: "303A", PID: "4001", Candidate: true},
		{Name: "COM3", VID: "1A86", PID: "55D3"},
	}
	if got := pickPort(ports); got != "COM4" {
		t.Fatalf("picked %s", got)
	}
	if got := pickPort(ports[2:]); got != "" {
		t.Fatalf("picked non-candidate %s", got)
	}
}
