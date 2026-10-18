// Package aircon is the air conditioner page: a Gree unit switched by
// infrared through the screen. Infrared only goes one way, so the page
// shows the settings last sent, or last heard from the unit's own remote
// through the screen's receiver, and keeps them across restarts.
package aircon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/since2006/go-dock/host/internal/config"
	"github.com/since2006/go-dock/host/internal/ir"
	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/plugins"
)

// ID is the page id in the protocol.
const ID = "aircon"

// Remote is the screen's infrared; *device.Manager implements it.
type Remote interface {
	// SendIR sends a frame, failing at once when the screen cannot.
	SendIR(hz int, raw []int) error
	// IRReady says why the screen cannot send now, or nil. It must not
	// block: the page asks it on the device link.
	IRReady() error
}

type Options struct {
	Enabled bool
	Name    string       // page title; empty is 空调
	Model   ir.GreeModel // remote to imitate; empty is YAW1F
}

// Modes in the order a tap goes through them.
var modeOrder = []ir.Mode{ir.ModeCool, ir.ModeHeat, ir.ModeDry, ir.ModeFan, ir.ModeAuto}

var (
	modeName = map[ir.Mode]string{ir.ModeAuto: "自动", ir.ModeCool: "制冷", ir.ModeDry: "除湿", ir.ModeFan: "送风", ir.ModeHeat: "制热"}
	modeIcon = map[ir.Mode]string{ir.ModeAuto: model.IconAuto, ir.ModeCool: model.IconCool, ir.ModeDry: model.IconDry, ir.ModeFan: model.IconFan, ir.ModeHeat: model.IconHeat}
	fanName  = map[ir.Fan]string{ir.FanAuto: "自动", ir.FanLow: "低", ir.FanMed: "中", ir.FanHigh: "高"}
)

type Plugin struct {
	remote   Remote
	path     string // where the state is kept between runs
	log      *slog.Logger
	onChange func()

	doMu   sync.Mutex // one change at a time: taps and the remote
	mu     sync.Mutex
	opts   Options
	state  ir.Gree
	hinted bool // said once that the remote looks like another model
}

// New creates a disabled plugin with the state saved at path, if any;
// SetOptions configures it.
func New(remote Remote, path string, log *slog.Logger, onChange func()) *Plugin {
	if onChange == nil {
		onChange = func() {}
	}
	p := &Plugin{remote: remote, path: path, log: log.With("component", "aircon"), onChange: onChange, state: ir.GreeDefault()}
	p.load()
	return p
}

func (p *Plugin) ID() string { return ID }

func (p *Plugin) SetOptions(o Options) {
	if o.Model == "" {
		o.Model = ir.GreeYAW1F
	}
	p.mu.Lock()
	if o.Model != p.opts.Model {
		p.hinted = false
	}
	p.opts = o
	p.mu.Unlock()
	p.onChange()
}

// describe puts a state in words for the page and the log.
func describe(g ir.Gree) string {
	if !g.Power() {
		return "已关机"
	}
	fan := fanName[g.Fan()] + "风"
	if g.Mode() == ir.ModeFan {
		return "送风 · " + fan
	}
	return fmt.Sprintf("%s %d°C · %s", modeName[g.Mode()], g.Temp(), fan)
}

// Page shows the power, temperature, mode and fan; all are greyed while
// the screen cannot send infrared. Auto mode holds the temperature and dry
// mode the fan, so those are greyed too.
func (p *Plugin) Page() (model.Page, bool) {
	down := p.remote.IRReady()
	p.mu.Lock()
	o, g := p.opts, p.state
	p.mu.Unlock()
	if !o.Enabled {
		return model.Page{}, false
	}
	title := strings.TrimSpace(o.Name)
	if title == "" {
		title = "空调"
	}
	pg := model.Page{ID: ID, Title: title, Summary: describe(g)}
	off := down != nil
	if off {
		pg.Error = down.Error()
	}
	on := g.Power()
	power := "已关"
	if on {
		power = "已开"
	}
	pg.Items = []model.PageItem{
		{ID: "power", Kind: model.KindSwitch, Label: "电源", Icon: model.IconPower, Text: power, On: &on, Disabled: off},
		{ID: "temp", Kind: model.KindStepper, Label: "温度", Text: fmt.Sprintf("%d°C", g.Temp()),
			AtMin: g.Temp() <= ir.GreeMinTemp, AtMax: g.Temp() >= ir.GreeMaxTemp, Disabled: off || g.Mode() == ir.ModeAuto},
		{ID: "mode", Kind: model.KindSelect, Label: "模式", Icon: modeIcon[g.Mode()], Text: modeName[g.Mode()], Disabled: off},
		{ID: "fan", Kind: model.KindSelect, Label: "风速", Icon: model.IconFan, Text: fanName[g.Fan()], Disabled: off || g.Mode() == ir.ModeDry},
	}
	return pg, true
}

// Do changes a setting. While the unit is on, every change sends the whole
// state, as its remote does; while off, changes are only kept and go out
// with the next power on.
func (p *Plugin) Do(ctx context.Context, item, act string) error {
	p.doMu.Lock()
	defer p.doMu.Unlock()
	p.mu.Lock()
	o, g := p.opts, p.state
	p.mu.Unlock()
	if !o.Enabled {
		return plugins.ErrNotFound
	}
	was := g
	switch item {
	case "power":
		switch act {
		case model.ActOn:
			g.SetPower(true)
		case model.ActOff:
			g.SetPower(false)
		case model.ActToggle:
			g.SetPower(!g.Power())
		default:
			return plugins.ErrBadAct
		}
	case "temp":
		switch act {
		case model.ActInc:
			g.SetTemp(g.Temp() + 1)
		case model.ActDec:
			g.SetTemp(g.Temp() - 1)
		default:
			return plugins.ErrBadAct
		}
	case "mode":
		if act != model.ActNext {
			return plugins.ErrBadAct
		}
		g.SetMode(nextMode(g.Mode()))
	case "fan":
		if act != model.ActNext {
			return plugins.ErrBadAct
		}
		g.SetFan((g.Fan() + 1) % (ir.FanHigh + 1))
	default:
		return plugins.ErrNotFound
	}
	if g == was {
		return nil // at a limit, or held by the mode
	}
	if g.Power() || was.Power() {
		frame := g.Frame(o.Model)
		if err := p.remote.SendIR(ir.Carrier, ir.GreeRaw(frame)); err != nil {
			p.log.Warn("空调红外发送失败", "err", err)
			return err
		}
		g = frame
		p.log.Info("已发送空调红外", "state", describe(g), "bytes", frame.String())
	}
	p.set(g)
	return nil
}

func nextMode(m ir.Mode) ir.Mode {
	for i, x := range modeOrder {
		if x == m {
			return modeOrder[(i+1)%len(modeOrder)]
		}
	}
	return modeOrder[0]
}

// Received takes a frame the screen's receiver heard: the unit's own
// remote keeps the page in step. Frames that are not Gree's are ignored.
func (p *Plugin) Received(raw []int) {
	g, err := ir.DecodeGree(raw)
	if err != nil {
		p.log.Debug("收到的红外信号不是格力空调的", "len", len(raw), "err", err)
		return
	}
	p.doMu.Lock()
	defer p.doMu.Unlock()
	p.mu.Lock()
	o, was := p.opts, p.state
	m, known := g.Model()
	hint := o.Enabled && known && m != o.Model && !p.hinted
	p.hinted = p.hinted || hint
	p.mu.Unlock()
	if !o.Enabled {
		return
	}
	if hint {
		p.log.Info("遥控器像是另一种格力型号，可在插件设置里改", "remote", strings.ToUpper(string(m)), "setting", strings.ToUpper(string(o.Model)))
	}
	if g.Frame(o.Model) == was.Frame(o.Model) {
		return
	}
	p.log.Info("收到格力遥控器信号", "state", describe(g), "bytes", g.String())
	p.set(g)
}

func (p *Plugin) set(g ir.Gree) {
	p.mu.Lock()
	p.state = g
	p.mu.Unlock()
	p.save(g)
	p.onChange()
}

type saved struct {
	Gree string `json:"gree"` // the 8 state bytes in hex
}

func (p *Plugin) load() {
	b, err := os.ReadFile(p.path)
	if err != nil {
		return // first run
	}
	var s saved
	if json.Unmarshal(b, &s) == nil {
		raw, err := hex.DecodeString(strings.ReplaceAll(s.Gree, " ", ""))
		if err == nil && len(raw) == len(p.state) {
			copy(p.state[:], raw)
			return
		}
	}
	p.log.Warn("空调状态文件读不懂，从默认状态开始", "path", p.path)
}

func (p *Plugin) save(g ir.Gree) {
	b, _ := json.Marshal(saved{Gree: g.String()})
	if err := config.WriteFileAtomic(p.path, append(b, '\n'), 0o600); err != nil {
		p.log.Warn("保存空调状态失败", "err", err)
	}
}
