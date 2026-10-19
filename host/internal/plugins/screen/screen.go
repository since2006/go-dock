// Package screen is the 屏幕 page: the screen's brightness, when it dims
// and muting the alert sounds, changed on the screen itself. The values live in
// go-dock's config, so a tap saves the config and the device link sends
// the screen its new settings, as a change on the web settings page does.
package screen

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/since2006/go-dock/host/internal/config"
	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/plugins"
)

// ID is the page id in the protocol.
const ID = "screen"

// The values the page steps through, as the web settings page lists them.
var (
	Brightness    = []int{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	DimBrightness = []int{0, 5, 10, 15, 20, 30, 40, 50}
	// DimAfter ends with 0, never.
	DimAfter = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour, 0}
)

// Device is the screen; *device.Manager implements it.
type Device interface {
	// SettingsReady says why the screen cannot take brightness settings,
	// or nil. It must not block: the page asks it on the device link.
	SettingsReady() error
}

// Save changes the device section of the config and applies it.
type Save func(func(*config.DeviceConfig)) error

type Options struct {
	Enabled bool
	// The current device config.
	Brightness    int
	DimBrightness int
	DimAfter      time.Duration
	Alerts        bool // an alert sound is on
	Mute          bool
}

type Plugin struct {
	dev      Device
	save     Save
	log      *slog.Logger
	onChange func()

	mu   sync.Mutex
	opts Options
}

// New creates a disabled plugin; SetOptions configures it.
func New(dev Device, save Save, log *slog.Logger, onChange func()) *Plugin {
	if onChange == nil {
		onChange = func() {}
	}
	return &Plugin{dev: dev, save: save, log: log.With("component", "screen"), onChange: onChange}
}

func (p *Plugin) ID() string { return ID }

func (p *Plugin) SetOptions(o Options) {
	p.mu.Lock()
	p.opts = o
	p.mu.Unlock()
	p.onChange()
}

// Page shows the brightness, the brightness when dimmed, when it dims and
// the sound switch, which mutes the alerts. The brightness cards are greyed
// while the screen's firmware cannot take them, the dimmed brightness also
// while it never dims, the sound while no alert is on.
func (p *Plugin) Page() (model.Page, bool) {
	down := p.dev.SettingsReady()
	p.mu.Lock()
	o := p.opts
	p.mu.Unlock()
	if !o.Enabled {
		return model.Page{}, false
	}
	pg := model.Page{ID: ID, Title: "屏幕"}
	off := down != nil
	if off {
		pg.Error = down.Error()
	}
	sound, on := "已开", !o.Mute
	switch {
	case !o.Alerts:
		sound = "未开启"
	case o.Mute:
		sound = "已静音"
	}
	pg.Items = []model.PageItem{
		{ID: "bright", Kind: model.KindStepper, Label: "亮度", Text: fmt.Sprintf("%d%%", o.Brightness),
			AtMin: o.Brightness <= Brightness[0], AtMax: o.Brightness >= Brightness[len(Brightness)-1], Disabled: off},
		{ID: "dim", Kind: model.KindStepper, Label: "变暗后亮度", Text: DimText(o.DimBrightness),
			AtMin: o.DimBrightness <= DimBrightness[0], AtMax: o.DimBrightness >= DimBrightness[len(DimBrightness)-1],
			Disabled: off || o.DimAfter == 0},
		{ID: "dim_after", Kind: model.KindSelect, Label: "无任务后变暗", Icon: model.IconMoon, Text: AfterText(o.DimAfter), Disabled: off},
		{ID: "beep", Kind: model.KindSwitch, Label: "提示音", Icon: model.IconBell, Text: sound, On: &on, Disabled: !o.Alerts},
	}
	return pg, true
}

// DimText names a dimmed brightness: 0 turns the screen off.
func DimText(pct int) string {
	if pct == 0 {
		return "熄屏"
	}
	return fmt.Sprintf("%d%%", pct)
}

// AfterText names a dim delay: 0 never dims.
func AfterText(d time.Duration) string {
	switch {
	case d == 0:
		return "从不"
	case d%time.Hour == 0:
		return fmt.Sprintf("%d 小时", d/time.Hour)
	default:
		return fmt.Sprintf("%d 分钟", d/time.Minute)
	}
}

// Do changes a setting and saves the config; the page follows from it.
func (p *Plugin) Do(ctx context.Context, item, act string) error {
	p.mu.Lock()
	o := p.opts
	p.mu.Unlock()
	if !o.Enabled {
		return plugins.ErrNotFound
	}
	var change func(*config.DeviceConfig)
	switch item {
	case "bright", "dim":
		if act != model.ActInc && act != model.ActDec {
			return plugins.ErrBadAct
		}
		levels, field := Brightness, func(d *config.DeviceConfig) *int { return &d.Brightness }
		if item == "dim" {
			levels, field = DimBrightness, func(d *config.DeviceConfig) *int { return &d.DimBrightness }
		}
		change = func(d *config.DeviceConfig) { *field(d) = step(levels, *field(d), act == model.ActInc) }
	case "dim_after":
		if act != model.ActNext {
			return plugins.ErrBadAct
		}
		change = func(d *config.DeviceConfig) { d.DimAfter = config.Duration(next(DimAfter, d.DimAfter.D())) }
	case "beep":
		switch act {
		case model.ActOn, model.ActOff, model.ActToggle:
		default:
			return plugins.ErrBadAct
		}
		change = func(d *config.DeviceConfig) { d.Mute = act == model.ActOff || (act == model.ActToggle && !d.Mute) }
	default:
		return plugins.ErrNotFound
	}
	if item != "beep" {
		if err := p.dev.SettingsReady(); err != nil {
			return err
		}
	}
	var now config.DeviceConfig
	err := p.save(func(d *config.DeviceConfig) {
		change(d)
		now = *d
	})
	if err != nil {
		p.log.Warn("保存屏幕设置失败", "item", item, "err", err)
		return err
	}
	p.log.Info("屏幕设置已修改", "brightness", now.Brightness, "dim_brightness", now.DimBrightness, "dim_after", now.DimAfter.String(), "mute", now.Mute)
	return nil
}

// step moves v to the next value in levels above it (up) or below it; at
// the end it stays.
func step(levels []int, v int, up bool) int {
	if up {
		if i := slices.IndexFunc(levels, func(l int) bool { return l > v }); i >= 0 {
			return levels[i]
		}
		return v
	}
	for i := len(levels) - 1; i >= 0; i-- {
		if levels[i] < v {
			return levels[i]
		}
	}
	return v
}

// next takes the delay after d in the list, wrapping round; one not in the
// list goes to the first.
func next(list []time.Duration, d time.Duration) time.Duration {
	i := slices.Index(list, d)
	return list[(i+1)%len(list)]
}
