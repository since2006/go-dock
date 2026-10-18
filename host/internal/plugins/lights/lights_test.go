package lights

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/since2006/go-dock/host/internal/config"
	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/plugins"
)

type sent struct{ topic, payload string }

// fakeBus records subscriptions and publishes; echo, if set, answers a
// publish synchronously, before Publish returns, like a fast lamp.
type fakeBus struct {
	mu        sync.Mutex
	connected bool
	subs      map[string][]func(string, []byte)
	unsubs    []string
	sent      []sent
	hooks     []func()
	pubErr    error
	echo      func(b *fakeBus, topic, payload string)
}

func newFakeBus() *fakeBus {
	return &fakeBus{connected: true, subs: map[string][]func(string, []byte){}}
}

func (b *fakeBus) Subscribe(filter string, h func(string, []byte)) func() {
	b.mu.Lock()
	b.subs[filter] = append(b.subs[filter], h)
	n := len(b.subs[filter]) - 1
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		b.subs[filter][n] = nil
		b.unsubs = append(b.unsubs, filter)
		b.mu.Unlock()
	}
}

func (b *fakeBus) Publish(ctx context.Context, topic string, payload []byte, retain bool) error {
	b.mu.Lock()
	err := b.pubErr
	if err == nil {
		b.sent = append(b.sent, sent{topic, string(payload)})
	}
	echo := b.echo
	b.mu.Unlock()
	if err == nil && echo != nil {
		echo(b, topic, string(payload))
	}
	return err
}

func (b *fakeBus) set(f func(b *fakeBus)) {
	b.mu.Lock()
	f(b)
	b.mu.Unlock()
}

func (b *fakeBus) Connected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connected
}

func (b *fakeBus) OnConnect(f func()) func() {
	b.mu.Lock()
	b.hooks = append(b.hooks, f)
	n := len(b.hooks) - 1
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		b.hooks[n] = nil
		b.mu.Unlock()
	}
}

// connect runs the live connect hooks, as after a (re)connect.
func (b *fakeBus) connect() {
	b.mu.Lock()
	hs := append([]func(){}, b.hooks...)
	b.mu.Unlock()
	for _, h := range hs {
		if h != nil {
			h()
		}
	}
}

// deliver hands a message to the live handlers of topic.
func (b *fakeBus) deliver(topic, payload string) {
	b.mu.Lock()
	hs := append([]func(string, []byte){}, b.subs[topic]...)
	b.mu.Unlock()
	for _, h := range hs {
		if h != nil {
			h(topic, []byte(payload))
		}
	}
}

func (b *fakeBus) live(topic string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, h := range b.subs[topic] {
		if h != nil {
			n++
		}
	}
	return n
}

func (b *fakeBus) sentList() []sent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]sent(nil), b.sent...)
}

// newTestPlugin puts lamps without a broker on "office", served by bus.
func newTestPlugin(t *testing.T, bus *fakeBus, ls ...config.Light) (*Plugin, *atomic.Int32) {
	t.Helper()
	return newMultiPlugin(t, map[string]*fakeBus{"office": bus}, ls...)
}

func newMultiPlugin(t *testing.T, buses map[string]*fakeBus, ls ...config.Light) (*Plugin, *atomic.Int32) {
	t.Helper()
	changes := &atomic.Int32{}
	p := New(func(id string) Bus {
		b := buses[id]
		if b == nil {
			t.Fatalf("no bus %q", id)
		}
		return b
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), func() { changes.Add(1) })
	p.pendingTimeout, p.tapGap = 30*time.Millisecond, 0
	p.SetOptions(opts(true, ls...))
	return p, changes
}

func opts(enabled bool, ls ...config.Light) Options {
	o := Options{Enabled: enabled, Brokers: map[string]string{"office": "办公室", "home": "家"}}
	for _, l := range ls {
		if l.Broker == "" {
			l.Broker = "office"
		}
		o.Lights = append(o.Lights, l)
	}
	return o
}

func waitSent(t *testing.T, b *fakeBus, want sent) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !slices.Contains(b.sentList(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("never sent %+v: %+v", want, b.sentList())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func item(t *testing.T, p *Plugin, id string) model.PageItem {
	t.Helper()
	pg, ok := p.Page()
	if !ok {
		t.Fatal("page hidden")
	}
	for _, it := range pg.Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("no item %q in %+v", id, pg)
	return model.PageItem{}
}

func mainLight() config.Light {
	return config.Light{ID: "main", Name: "主灯", CommandTopic: "office/main/set", StateTopic: "office/main/state"}
}

func TestParseState(t *testing.T) {
	raw := withDefaults(config.Light{})
	z2m := withDefaults(config.Light{PayloadOn: `{"state":"ON"}`, PayloadOff: `{"state":"OFF"}`, StatePath: "state"})
	shelly := withDefaults(config.Light{StatePath: "output"})
	ones := withDefaults(config.Light{StateOn: "1", StateOff: "0"})
	for _, tc := range []struct {
		name    string
		c       config.Light
		payload string
		on, ok  bool
	}{
		{"raw on", raw, "ON", true, true},
		{"raw lower, spaces", raw, " off \n", false, true},
		{"raw unknown", raw, "TOGGLE", false, false},
		{"json path", z2m, `{"state":"ON","brightness":200}`, true, true},
		{"json path off", z2m, `{"state":"off"}`, false, true},
		{"json missing field", z2m, `{"brightness":200}`, false, false},
		{"bad json", z2m, `state=ON`, false, false},
		{"json bool", shelly, `{"id":0,"output":true}`, true, true},
		{"json bool off", shelly, `{"output":false}`, false, true},
		{"custom values", ones, "1", true, true},
	} {
		on, ok := parseState(tc.c, []byte(tc.payload))
		if on != tc.on || ok != tc.ok {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", tc.name, on, ok, tc.on, tc.ok)
		}
	}
	if z2m.StateOn != "ON" || raw.StateOn != "ON" || raw.PayloadOff != "OFF" {
		t.Fatalf("defaults %+v %+v", z2m, raw)
	}
}

func TestToggleWithStateReport(t *testing.T) {
	bus := newFakeBus()
	p, changes := newTestPlugin(t, bus, mainLight())
	if bus.live("office/main/state") != 1 {
		t.Fatal("state topic not subscribed")
	}
	if it := item(t, p, "main"); it.Text != textUnknown || it.On != nil || it.Busy {
		t.Fatalf("initial %+v", it)
	}

	// Unknown: toggle turns it on and waits for the report.
	if err := p.Do(context.Background(), "main", model.ActToggle); err != nil {
		t.Fatal(err)
	}
	if s := bus.sentList(); len(s) != 1 || s[0] != (sent{"office/main/set", "ON"}) {
		t.Fatalf("sent %+v", s)
	}
	if it := item(t, p, "main"); !it.Busy || it.Text != textBusy {
		t.Fatalf("after toggle %+v", it)
	}
	// A pending command decides the next toggle.
	if err := p.Do(context.Background(), "main", model.ActToggle); err != nil {
		t.Fatal(err)
	}
	if s := bus.sentList(); s[1].payload != "OFF" {
		t.Fatalf("second toggle sent %+v", s)
	}
	before := changes.Load()
	bus.deliver("office/main/state", "OFF")
	it := item(t, p, "main")
	if it.Busy || it.Text != textOff || it.On == nil || *it.On {
		t.Fatalf("after report %+v", it)
	}
	if changes.Load() == before {
		t.Fatal("no change notification")
	}
	// The same state again changes nothing.
	before = changes.Load()
	bus.deliver("office/main/state", "OFF")
	if changes.Load() != before {
		t.Fatal("notified for an unchanged state")
	}
	// Known off: toggle turns it on.
	_ = p.Do(context.Background(), "main", model.ActToggle)
	if s := bus.sentList(); s[len(s)-1].payload != "ON" {
		t.Fatalf("toggle from off sent %+v", s)
	}
}

func TestFastReportBeforePublishReturns(t *testing.T) {
	bus := newFakeBus()
	bus.echo = func(b *fakeBus, topic, payload string) { b.deliver("office/main/state", payload) }
	p, _ := newTestPlugin(t, bus, mainLight())
	if err := p.Do(context.Background(), "main", model.ActOn); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond) // past the pending timeout
	if it := item(t, p, "main"); it.Busy || it.Text != textOn {
		t.Fatalf("%+v", it)
	}
}

func TestNoReply(t *testing.T) {
	bus := newFakeBus()
	p, changes := newTestPlugin(t, bus, mainLight())
	before := changes.Load()
	_ = p.Do(context.Background(), "main", model.ActOn)
	deadline := time.Now().Add(time.Second)
	for item(t, p, "main").Text != textNoReply {
		if time.Now().After(deadline) {
			t.Fatalf("still %+v", item(t, p, "main"))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if changes.Load() < before+3 { // busy, published, timed out
		t.Fatalf("changes %d -> %d", before, changes.Load())
	}
	// A late report clears it.
	bus.deliver("office/main/state", "ON")
	if it := item(t, p, "main"); it.Text != textOn {
		t.Fatalf("%+v", it)
	}
}

func TestOptimisticWithoutStateTopic(t *testing.T) {
	bus := newFakeBus()
	l := mainLight()
	l.StateTopic = ""
	p, _ := newTestPlugin(t, bus, l)
	_ = p.Do(context.Background(), "main", model.ActOn)
	if it := item(t, p, "main"); it.Busy || it.Text != textOn {
		t.Fatalf("%+v", it)
	}
	_ = p.Do(context.Background(), "main", model.ActToggle)
	if it := item(t, p, "main"); it.Text != textOff {
		t.Fatalf("%+v", it)
	}
}

func TestPublishFailureAndOffline(t *testing.T) {
	bus := newFakeBus()
	p, _ := newTestPlugin(t, bus, mainLight())
	bus.set(func(b *fakeBus) { b.pubErr = errors.New("boom") })
	if err := p.Do(context.Background(), "main", model.ActOn); err == nil {
		t.Fatal("no error")
	}
	if it := item(t, p, "main"); it.Busy || it.Text != textFailed {
		t.Fatalf("%+v", it)
	}

	bus.set(func(b *fakeBus) { b.pubErr, b.connected = nil, false })
	if err := p.Do(context.Background(), "main", model.ActOn); !errors.Is(err, errOffline) {
		t.Fatalf("offline: %v", err)
	}
	pg, _ := p.Page()
	if pg.Error != "MQTT 未连接" || !pg.Items[0].Disabled {
		t.Fatalf("offline page %+v", pg)
	}
	if n := len(bus.sentList()); n != 0 {
		t.Fatalf("sent %d while offline", n)
	}
}

func TestDoErrors(t *testing.T) {
	bus := newFakeBus()
	p, _ := newTestPlugin(t, bus, mainLight())
	if err := p.Do(context.Background(), "nope", model.ActOn); !errors.Is(err, plugins.ErrNotFound) {
		t.Fatalf("unknown item: %v", err)
	}
	if err := p.Do(context.Background(), "main", "dim"); !errors.Is(err, plugins.ErrBadAct) {
		t.Fatalf("bad act: %v", err)
	}
}

func TestDoubleTap(t *testing.T) {
	bus := newFakeBus()
	p, _ := newTestPlugin(t, bus, mainLight())
	p.tapGap = time.Hour
	_ = p.Do(context.Background(), "main", model.ActToggle)
	_ = p.Do(context.Background(), "main", model.ActToggle)
	if n := len(bus.sentList()); n != 1 {
		t.Fatalf("sent %d commands for a double tap", n)
	}
}

func TestSetOptions(t *testing.T) {
	bus := newFakeBus()
	desk := config.Light{ID: "desk", Name: "台灯", CommandTopic: "office/desk/set", StateTopic: "office/desk/state"}
	p, _ := newTestPlugin(t, bus, mainLight(), desk)
	bus.deliver("office/main/state", "ON")
	bus.deliver("office/desk/state", "ON")

	// Renaming keeps the state and the subscription; a new state topic
	// starts over; a removed lamp is unsubscribed.
	m := mainLight()
	m.Name = "吸顶灯"
	m.CommandTopic = "office/main/cmd"
	desk.StateTopic = "office/desk/status"
	p.SetOptions(opts(true, m, desk))
	if it := item(t, p, "main"); it.Label != "吸顶灯" || it.Text != textOn {
		t.Fatalf("main %+v", it)
	}
	if bus.live("office/main/state") != 1 {
		t.Fatal("main resubscribed")
	}
	if it := item(t, p, "desk"); it.Text != textUnknown {
		t.Fatalf("desk %+v", it)
	}
	if bus.live("office/desk/state") != 0 || bus.live("office/desk/status") != 1 {
		t.Fatal("desk subscriptions not moved")
	}
	_ = p.Do(context.Background(), "main", model.ActOff)
	if s := bus.sentList(); s[len(s)-1].topic != "office/main/cmd" {
		t.Fatalf("sent %+v", s)
	}

	p.SetOptions(opts(true, m))
	if bus.live("office/desk/status") != 0 {
		t.Fatal("removed lamp still subscribed")
	}
	pg, _ := p.Page()
	if len(pg.Items) != 1 {
		t.Fatalf("items %+v", pg.Items)
	}

	p.SetOptions(opts(false, m))
	if _, ok := p.Page(); ok {
		t.Fatal("disabled page shown")
	}
	if bus.live("office/main/state") != 0 {
		t.Fatal("disabled plugin still subscribed")
	}
}

func TestQueryOnConnect(t *testing.T) {
	bus := newFakeBus()
	l := mainLight()
	l.QueryTopic, l.QueryPayload = "office/main/get", `{"state":""}`
	newTestPlugin(t, bus, l, config.Light{ID: "desk", Name: "台灯", CommandTopic: "x"})
	// Added while connected: asked right away, in the background.
	deadline := time.Now().Add(time.Second)
	for len(bus.sentList()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no query")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// And again on every connect.
	bus.connect()
	s := bus.sentList()
	if len(s) != 2 || s[1] != (sent{"office/main/get", `{"state":""}`}) {
		t.Fatalf("sent %+v", s)
	}
}

func TestPageSummary(t *testing.T) {
	bus := newFakeBus()
	desk := config.Light{ID: "desk", Name: "台灯", CommandTopic: "office/desk/set", StateTopic: "office/desk/state"}
	fan := config.Light{ID: "fan", Name: "风扇灯", CommandTopic: "office/fan/set", StateTopic: "office/fan/state"}
	p, _ := newTestPlugin(t, bus, mainLight(), desk, fan)
	bus.deliver("office/main/state", "ON")
	bus.deliver("office/desk/state", "OFF")
	pg, ok := p.Page()
	if !ok || pg.ID != ID || pg.Title != "灯光" || pg.Summary != "1 / 3 盏亮着 · 1 盏未知" || pg.Error != "" {
		t.Fatalf("%+v", pg)
	}
	if pg.Items[0].ID != "main" || pg.Items[2].ID != "fan" {
		t.Fatalf("order %+v", pg.Items)
	}

	empty, _ := newTestPlugin(t, newFakeBus())
	if _, ok := empty.Page(); ok {
		t.Fatal("page without lamps shown")
	}
}

func TestTwoBrokers(t *testing.T) {
	office, home := newFakeBus(), newFakeBus()
	home.set(func(b *fakeBus) { b.connected = false })
	desk := config.Light{ID: "desk", Name: "台灯", Broker: "home", CommandTopic: "home/desk/set", StateTopic: "home/desk/state", QueryTopic: "home/desk/get"}
	m := mainLight()
	m.QueryTopic = "office/main/get"
	p, _ := newMultiPlugin(t, map[string]*fakeBus{"office": office, "home": home}, m, desk)

	if office.live("office/main/state") != 1 || home.live("home/desk/state") != 1 || office.live("home/desk/state") != 0 {
		t.Fatal("lamps subscribed on the wrong broker")
	}
	// Only the lamps on a down broker are disabled, and the page says which.
	pg, _ := p.Page()
	if pg.Error != "家 未连接" || pg.Items[0].Disabled || !pg.Items[1].Disabled {
		t.Fatalf("page %+v", pg)
	}
	if err := p.Do(context.Background(), "desk", model.ActOn); !errors.Is(err, errOffline) || err.Error() != "MQTT 未连接（家）" {
		t.Fatalf("desk: %v", err)
	}
	if err := p.Do(context.Background(), "main", model.ActOn); err != nil {
		t.Fatal(err)
	}
	// The query of the newly added lamp goes out in the background.
	waitSent(t, office, sent{"office/main/get", ""})
	if s := office.sentList(); !slices.Contains(s, sent{"office/main/set", "ON"}) {
		t.Fatalf("office sent %+v", s)
	}

	// A broker coming up asks only its own lamps.
	home.set(func(b *fakeBus) { b.connected = true })
	before := len(office.sentList())
	home.connect()
	if s := home.sentList(); len(s) != 1 || s[0].topic != "home/desk/get" {
		t.Fatalf("home sent %+v", s)
	}
	if len(office.sentList()) != before {
		t.Fatal("office lamps queried on home connect")
	}

	// Moving the desk lamp to the office broker resubscribes it there and
	// drops the home connect hook.
	desk.Broker = "office"
	p.SetOptions(opts(true, m, desk))
	if home.live("home/desk/state") != 0 || office.live("home/desk/state") != 1 {
		t.Fatal("desk lamp not moved")
	}
	home.connect()
	if len(home.sentList()) != 1 {
		t.Fatal("home hook still runs")
	}
	if pg, _ := p.Page(); pg.Error != "" {
		t.Fatalf("error %q", pg.Error)
	}
}

func TestNoBroker(t *testing.T) {
	p := New(func(id string) Bus { t.Fatalf("bus %q asked for", id); return nil }, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	p.SetOptions(Options{Enabled: true, Lights: []config.Light{mainLight()}})
	pg, ok := p.Page()
	if !ok || pg.Error != "没有配置 MQTT broker" || !pg.Items[0].Disabled {
		t.Fatalf("page %+v", pg)
	}
	if err := p.Do(context.Background(), "main", model.ActOn); !errors.Is(err, errNoBroker) {
		t.Fatalf("do: %v", err)
	}
}
