// Package lights is the lights page: lamps switched over MQTT, each on a
// broker of its own choosing, with a command topic and, optionally, a state
// topic that reports what the lamp really did.
package lights

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"

	"github.com/since2006/go-dock/host/internal/config"
	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/plugins"
)

// ID is the page id in the protocol.
const ID = "lights"

// Bus is the part of the MQTT client the plugin uses; *mqtt.Client
// implements it.
type Bus interface {
	Subscribe(filter string, h func(topic string, payload []byte)) (unsubscribe func())
	Publish(ctx context.Context, topic string, payload []byte, retain bool) error
	Connected() bool
	OnConnect(f func()) (remove func())
}

type Options struct {
	Enabled bool
	// Lights carry their broker id resolved (config.MQTTConfig.BrokerFor);
	// empty means no broker is configured.
	Lights []config.Light
	// Brokers names the brokers by id, for messages.
	Brokers map[string]string
}

var (
	errOffline  = errors.New("MQTT 未连接")
	errNoBroker = errors.New("没有配置 MQTT broker")
)

// Item texts.
const (
	textOn      = "已开"
	textOff     = "已关"
	textUnknown = "未知"
	textBusy    = "切换中…"
	textNoReply = "未响应"
	textFailed  = "发送失败"
)

type Plugin struct {
	bus      func(broker string) Bus
	log      *slog.Logger
	onChange func()

	// Replaceable in tests.
	pendingTimeout time.Duration // a command without a state report fails
	tapGap         time.Duration // repeated toggles closer than this are one tap

	setMu   sync.Mutex // serializes SetOptions
	mu      sync.Mutex
	enabled bool
	lamps   []*lamp
	names   map[string]string
	hooks   map[string]func() // removes the connect hook, per broker in use
}

type lamp struct {
	cfg     config.Light // with defaults filled in
	bus     Bus          // nil without a broker
	on      *bool        // last reported (or, without a state topic, commanded) state
	pending *bool        // commanded state awaiting a report
	gen     int          // bumped by every command, so stale timers do nothing
	failed  string       // textNoReply or textFailed until the next report or command
	logged  string       // what the last state message said: "true", "false" or "?"
	lastTap time.Time
	unsub   func()
}

// New creates a disabled plugin; SetOptions configures it. bus returns the
// connection to a broker by id.
func New(bus func(broker string) Bus, log *slog.Logger, onChange func()) *Plugin {
	if onChange == nil {
		onChange = func() {}
	}
	return &Plugin{
		bus:            bus,
		log:            log.With("component", "lights"),
		onChange:       onChange,
		pendingTimeout: 5 * time.Second,
		tapGap:         300 * time.Millisecond,
		hooks:          map[string]func(){},
	}
}

func (p *Plugin) ID() string { return ID }

// withDefaults fills in the ON/OFF payloads and the state values. With a
// JSON path the state is usually a word like "ON" even when the command
// is a JSON object, so the payloads only stand in for raw states.
func withDefaults(l config.Light) config.Light {
	if l.PayloadOn == "" {
		l.PayloadOn = "ON"
	}
	if l.PayloadOff == "" {
		l.PayloadOff = "OFF"
	}
	if l.StateOn == "" {
		l.StateOn = l.PayloadOn
		if l.StatePath != "" {
			l.StateOn = "ON"
		}
	}
	if l.StateOff == "" {
		l.StateOff = l.PayloadOff
		if l.StatePath != "" {
			l.StateOff = "OFF"
		}
	}
	return l
}

// sameState reports whether a and b read the state the same way, so a
// lamp keeps its known state and subscription across the change.
func sameState(a, b config.Light) bool {
	return a.Broker == b.Broker && a.StateTopic == b.StateTopic && a.StatePath == b.StatePath &&
		a.StateOn == b.StateOn && a.StateOff == b.StateOff
}

// SetOptions applies a new lamp list. Lamps whose broker and state settings
// did not change keep their state; the others are subscribed afresh.
func (p *Plugin) SetOptions(o Options) {
	p.setMu.Lock()
	defer p.setMu.Unlock()

	p.mu.Lock()
	old := map[string]*lamp{}
	for _, l := range p.lamps {
		old[l.cfg.ID] = l
	}
	var next, fresh []*lamp
	var drop []func()
	if o.Enabled {
		for _, c := range o.Lights {
			c = withDefaults(c)
			if l, ok := old[c.ID]; ok && sameState(l.cfg, c) {
				l.cfg = c
				next = append(next, l)
				delete(old, c.ID)
				continue
			}
			l := &lamp{cfg: c}
			if c.Broker != "" {
				l.bus = p.bus(c.Broker)
			}
			next = append(next, l)
			fresh = append(fresh, l)
		}
	}
	for _, l := range old {
		l.gen++ // its pending timer, if any, does nothing now
		if l.unsub != nil {
			drop = append(drop, l.unsub)
		}
	}
	p.enabled = o.Enabled
	p.lamps = next
	p.names = o.Brokers

	// One connect hook per broker in use, to ask its lamps for their state.
	used := map[string]bool{}
	for _, l := range next {
		if l.bus != nil {
			used[l.cfg.Broker] = true
		}
	}
	var unhook []func()
	var hook []string
	for id, remove := range p.hooks {
		if !used[id] {
			unhook = append(unhook, remove)
			delete(p.hooks, id)
		}
	}
	for id := range used {
		if p.hooks[id] == nil {
			hook = append(hook, id)
		}
	}
	p.mu.Unlock()

	for _, unsub := range drop {
		unsub()
	}
	for _, remove := range unhook {
		remove()
	}
	for _, id := range hook {
		remove := p.bus(id).OnConnect(func() { p.query(id) })
		p.mu.Lock()
		p.hooks[id] = remove
		p.mu.Unlock()
	}
	var ask []*lamp
	for _, l := range fresh {
		if l.bus == nil {
			continue
		}
		if l.cfg.StateTopic != "" {
			unsub := l.bus.Subscribe(l.cfg.StateTopic, p.onState(l))
			p.mu.Lock()
			l.unsub = unsub
			p.mu.Unlock()
		}
		if l.bus.Connected() {
			ask = append(ask, l)
		}
	}
	if len(ask) > 0 {
		go p.queryLamps(ask)
	}
	p.onChange()
}

// parseState reads a lamp's state from a state-topic payload: the whole
// payload, or the value at StatePath in JSON. JSON booleans count as is.
func parseState(c config.Light, payload []byte) (on, ok bool) {
	v := string(payload)
	if c.StatePath != "" {
		if !gjson.ValidBytes(payload) {
			return false, false
		}
		r := gjson.GetBytes(payload, c.StatePath)
		switch r.Type {
		case gjson.True:
			return true, true
		case gjson.False:
			return false, true
		}
		if !r.Exists() {
			return false, false
		}
		v = r.String()
	}
	v = strings.TrimSpace(v)
	switch {
	case strings.EqualFold(v, strings.TrimSpace(c.StateOn)):
		return true, true
	case strings.EqualFold(v, strings.TrimSpace(c.StateOff)):
		return false, true
	}
	return false, false
}

// logPayload is a payload for the log; logx shows JSON as is.
func logPayload(b []byte) string {
	if len(b) > 2048 {
		return strings.ToValidUTF8(string(b[:2048]), "") + "…"
	}
	return string(b)
}

func (p *Plugin) onState(l *lamp) func(topic string, payload []byte) {
	return func(topic string, payload []byte) {
		p.mu.Lock()
		c := l.cfg
		on, ok := parseState(c, payload)
		// Repeats go to the debug log only: some devices republish an
		// unchanged state every few seconds.
		said := "?"
		if ok {
			said = strconv.FormatBool(on)
		}
		level := slog.LevelDebug
		if said != l.logged || (ok && l.pending != nil) {
			level = slog.LevelInfo
		}
		l.logged = said
		if !ok {
			p.mu.Unlock()
			attrs := []any{"light", c.Name, "topic", topic, "payload", logPayload(payload)}
			if c.StatePath != "" {
				attrs = append(attrs, "state_path", c.StatePath)
			}
			attrs = append(attrs, "state_on", c.StateOn, "state_off", c.StateOff)
			p.log.Log(context.Background(), level, "无法识别灯的状态，请检查状态路径和状态值", attrs...)
			return
		}
		before := p.item(l, true)
		l.on = &on
		l.failed = ""
		if l.pending != nil && *l.pending == on {
			l.pending = nil
			l.gen++
		}
		changed := !sameItem(before, p.item(l, true))
		p.mu.Unlock()
		p.log.Log(context.Background(), level, "收到灯的状态", "light", c.Name, "on", on, "topic", topic, "payload", logPayload(payload))
		if changed {
			p.onChange()
		}
	}
}

// query asks the lamps on broker to report their state, after it
// connects.
func (p *Plugin) query(broker string) {
	p.mu.Lock()
	var ls []*lamp
	for _, l := range p.lamps {
		if l.cfg.Broker == broker {
			ls = append(ls, l)
		}
	}
	p.mu.Unlock()
	p.queryLamps(ls)
}

// queryLamps publishes the query of each lamp that has one.
func (p *Plugin) queryLamps(ls []*lamp) {
	type q struct {
		bus    Bus
		cfg    config.Light
		broker string
	}
	var qs []q
	p.mu.Lock()
	for _, l := range ls {
		if l.bus != nil && l.cfg.QueryTopic != "" {
			qs = append(qs, q{l.bus, l.cfg, p.label(l.cfg.Broker)})
		}
	}
	p.mu.Unlock()
	for _, x := range qs {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := x.bus.Publish(ctx, x.cfg.QueryTopic, []byte(x.cfg.QueryPayload), false)
		cancel()
		if err != nil {
			p.log.Warn("查询灯的状态失败", "light", x.cfg.Name, "broker", x.broker, "topic", x.cfg.QueryTopic, "err", err)
			continue
		}
		p.log.Info("已查询灯的状态", "light", x.cfg.Name, "broker", x.broker, "topic", x.cfg.QueryTopic, "payload", logPayload([]byte(x.cfg.QueryPayload)))
	}
}

// label names a broker in messages.
func (p *Plugin) label(broker string) string {
	if n := p.names[broker]; n != "" {
		return n
	}
	return broker
}

func (p *Plugin) find(id string) *lamp {
	for _, l := range p.lamps {
		if l.cfg.ID == id {
			return l
		}
	}
	return nil
}

// Do switches a lamp: act is on, off or toggle. Toggling an unknown lamp
// turns it on.
func (p *Plugin) Do(ctx context.Context, item, act string) error {
	p.mu.Lock()
	l := p.find(item)
	if !p.enabled || l == nil {
		p.mu.Unlock()
		return plugins.ErrNotFound
	}
	if l.bus == nil {
		p.mu.Unlock()
		return errNoBroker
	}
	if !l.bus.Connected() {
		name := p.label(l.cfg.Broker)
		p.mu.Unlock()
		return fmt.Errorf("%w（%s）", errOffline, name)
	}
	var target bool
	switch act {
	case model.ActOn:
		target = true
	case model.ActOff:
	case model.ActToggle:
		if time.Since(l.lastTap) < p.tapGap {
			p.mu.Unlock()
			return nil // a double tap
		}
		cur := l.on
		if l.pending != nil {
			cur = l.pending
		}
		target = cur == nil || !*cur
	default:
		p.mu.Unlock()
		return plugins.ErrBadAct
	}
	l.lastTap = time.Now()
	c, bus, broker := l.cfg, l.bus, p.label(l.cfg.Broker)
	l.gen++
	gen := l.gen
	l.failed = ""
	if c.StateTopic != "" {
		// Pending before the publish: a fast lamp may report back before
		// Publish even returns.
		l.pending = &target
		time.AfterFunc(p.pendingTimeout, func() { p.expire(l, gen) })
	}
	p.mu.Unlock()
	p.onChange()

	payload := c.PayloadOff
	if target {
		payload = c.PayloadOn
	}
	err := bus.Publish(ctx, c.CommandTopic, []byte(payload), false)

	p.mu.Lock()
	if l.gen == gen {
		switch {
		case err != nil:
			l.pending, l.failed = nil, textFailed
			l.gen++
		case c.StateTopic == "":
			l.on = &target // nothing will report back
		}
	}
	p.mu.Unlock()
	p.onChange()
	if err != nil {
		p.log.Warn("开关灯失败", "light", c.Name, "broker", broker, "topic", c.CommandTopic, "err", err)
		return err
	}
	p.log.Info("已发送开关灯命令", "light", c.Name, "on", target, "broker", broker, "topic", c.CommandTopic, "payload", logPayload([]byte(payload)))
	return nil
}

func (p *Plugin) expire(l *lamp, gen int) {
	p.mu.Lock()
	if l.gen != gen || l.pending == nil {
		p.mu.Unlock()
		return
	}
	l.pending, l.failed = nil, textNoReply
	l.gen++
	name := l.cfg.Name
	p.mu.Unlock()
	p.log.Warn("灯没有回报状态，请检查状态主题和状态值", "light", name)
	p.onChange()
}

// item renders one lamp; the caller holds mu.
func (p *Plugin) item(l *lamp, connected bool) model.PageItem {
	it := model.PageItem{ID: l.cfg.ID, Kind: model.KindSwitch, Label: l.cfg.Name, Disabled: !connected}
	if l.on != nil {
		v := *l.on
		it.On = &v
	}
	switch {
	case l.pending != nil:
		it.Busy, it.Text = true, textBusy
	case l.failed != "":
		it.Text = l.failed
	case l.on == nil:
		it.Text = textUnknown
	case *l.on:
		it.Text = textOn
	default:
		it.Text = textOff
	}
	return it
}

func sameItem(a, b model.PageItem) bool {
	ao, bo := a.On, b.On
	a.On, b.On = nil, nil
	return a == b && (ao == nil) == (bo == nil) && (ao == nil || *ao == *bo)
}

// Page shows every lamp; it is hidden while the plugin is off or has no
// lamps. Lamps whose broker is down are disabled, and the page says which
// brokers are down.
func (p *Plugin) Page() (model.Page, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.enabled || len(p.lamps) == 0 {
		return model.Page{}, false
	}
	pg := model.Page{ID: ID, Title: "灯光", Items: make([]model.PageItem, 0, len(p.lamps))}
	up := map[string]bool{} // per broker id, "" for none
	var down []string
	on, unknown := 0, 0
	for _, l := range p.lamps {
		id := l.cfg.Broker
		c, seen := up[id]
		if !seen {
			c = l.bus != nil && l.bus.Connected()
			up[id] = c
			if !c && l.bus != nil {
				down = append(down, p.label(id))
			}
		}
		pg.Items = append(pg.Items, p.item(l, c))
		switch {
		case l.on == nil:
			unknown++
		case *l.on:
			on++
		}
	}
	pg.Summary = fmt.Sprintf("%d / %d 盏亮着", on, len(p.lamps))
	if unknown > 0 {
		pg.Summary += fmt.Sprintf(" · %d 盏未知", unknown)
	}
	_, noBroker := up[""]
	switch {
	case noBroker:
		pg.Error = errNoBroker.Error()
	case len(down) == 0:
	case len(up) == 1:
		pg.Error = errOffline.Error()
	default:
		pg.Error = strings.Join(down, "、") + " 未连接"
	}
	return pg, true
}
