package screen

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/since2006/go-dock/host/internal/config"
	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/plugins"
)

type fakeDevice struct {
	mu  sync.Mutex
	err error
}

func (d *fakeDevice) SettingsReady() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

type fixture struct {
	p     *Plugin
	dev   *fakeDevice
	store *config.Store
	logs  *bytes.Buffer
}

// newFixture wires the plugin to a config store as the app does.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{dev: &fakeDevice{}, store: store, logs: &bytes.Buffer{}}
	log := slog.New(slog.NewTextHandler(f.logs, nil))
	f.p = New(f.dev, func(change func(*config.DeviceConfig)) error {
		return store.Update(func(c *config.Config) { change(&c.Device) })
	}, log, nil)
	apply := func(c config.Config) {
		d := c.Device
		f.p.SetOptions(Options{
			Enabled: d.SettingsPage, Brightness: d.Brightness, DimBrightness: d.DimBrightness, DimAfter: d.DimAfter.D(),
			Alerts: d.Beep || d.BeepDone, Mute: d.Mute,
		})
	}
	store.OnChange(func(_, cur config.Config) { apply(cur) })
	apply(store.Get())
	return f
}

func (f *fixture) do(t *testing.T, item, act string) {
	t.Helper()
	if err := f.p.Do(context.Background(), item, act); err != nil {
		t.Fatalf("%s %s: %v", item, act, err)
	}
}

func (f *fixture) item(t *testing.T, id string) model.PageItem {
	t.Helper()
	pg, ok := f.p.Page()
	if !ok {
		t.Fatal("page hidden")
	}
	for _, it := range pg.Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("no item %q", id)
	return model.PageItem{}
}

func TestPage(t *testing.T) {
	f := newFixture(t)
	pg, ok := f.p.Page()
	if !ok || pg.Title != "屏幕" || pg.Error != "" || len(pg.Items) != 4 {
		t.Fatalf("page %+v", pg)
	}
	want := []struct {
		id, kind, text, icon string
	}{
		{"bright", model.KindStepper, "80%", ""},
		{"dim", model.KindStepper, "20%", ""},
		{"dim_after", model.KindSelect, "5 分钟", model.IconMoon},
		{"beep", model.KindSwitch, "已开", model.IconBell},
	}
	for i, w := range want {
		it := pg.Items[i]
		if it.ID != w.id || it.Kind != w.kind || it.Text != w.text || it.Icon != w.icon || it.Disabled {
			t.Errorf("item %d = %+v, want %+v", i, it, w)
		}
	}
	if !*pg.Items[3].On {
		t.Error("sound shows muted")
	}

	cfg := f.store.Get()
	cfg.Device.SettingsPage = false
	if err := f.store.Set(cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.p.Page(); ok {
		t.Fatal("page shown while turned off")
	}
	if err := f.p.Do(context.Background(), "bright", model.ActInc); !errors.Is(err, plugins.ErrNotFound) {
		t.Fatalf("tap on a hidden page: %v", err)
	}
}

func TestSteppers(t *testing.T) {
	f := newFixture(t)
	f.do(t, "bright", model.ActInc)
	f.do(t, "bright", model.ActInc)
	if it := f.item(t, "bright"); it.Text != "100%" || !it.AtMax || it.AtMin {
		t.Fatalf("bright %+v", it)
	}
	f.do(t, "bright", model.ActInc) // at the end it stays
	if got := f.store.Get().Device.Brightness; got != 100 {
		t.Fatalf("brightness %d", got)
	}
	for range 12 {
		f.do(t, "dim", model.ActDec)
	}
	if it := f.item(t, "dim"); it.Text != "熄屏" || !it.AtMin {
		t.Fatalf("dim %+v", it)
	}
	f.do(t, "dim", model.ActInc)
	if got := f.store.Get().Device.DimBrightness; got != 5 {
		t.Fatalf("dim brightness %d", got)
	}
	if !strings.Contains(f.logs.String(), "屏幕设置已修改") {
		t.Fatal("not logged")
	}

	// A value off the list, written by hand, steps onto it.
	cfg := f.store.Get()
	cfg.Device.Brightness = 35
	f.store.Set(cfg)
	f.do(t, "bright", model.ActDec)
	if got := f.store.Get().Device.Brightness; got != 30 {
		t.Fatalf("35 down: %d", got)
	}
	if err := f.p.Do(context.Background(), "bright", model.ActNext); !errors.Is(err, plugins.ErrBadAct) {
		t.Fatalf("next on a stepper: %v", err)
	}
}

func TestDimAfter(t *testing.T) {
	f := newFixture(t)
	var seen []string
	for range len(DimAfter) {
		f.do(t, "dim_after", model.ActNext)
		seen = append(seen, f.item(t, "dim_after").Text)
	}
	if got := strings.Join(seen, ","); got != "10 分钟,30 分钟,1 小时,从不,1 分钟,2 分钟,5 分钟" {
		t.Fatalf("order %s", got)
	}

	// Never dimming greys the dimmed brightness; it is saved as 0.
	for f.item(t, "dim_after").Text != "从不" {
		f.do(t, "dim_after", model.ActNext)
	}
	if !f.item(t, "dim").Disabled || f.store.Get().Device.DimAfter != 0 {
		t.Fatal("never dimming")
	}
	if b, _ := config.Load(f.store.Path()); b.Get().Device.DimAfter != 0 {
		t.Fatal("not saved")
	}

	// A delay off the list goes to the first.
	cfg := f.store.Get()
	cfg.Device.DimAfter = config.Duration(3 * time.Minute)
	f.store.Set(cfg)
	f.do(t, "dim_after", model.ActNext)
	if got := f.store.Get().Device.DimAfter.D(); got != time.Minute {
		t.Fatalf("after 3m: %v", got)
	}
}

func TestMute(t *testing.T) {
	f := newFixture(t)
	f.do(t, "beep", model.ActToggle)
	d := f.store.Get().Device
	if it := f.item(t, "beep"); it.Text != "已静音" || *it.On || !d.Mute || !d.Beep {
		t.Fatalf("muted %+v, config %+v", it, d)
	}
	f.do(t, "beep", model.ActOn)
	f.do(t, "beep", model.ActOn)
	if f.store.Get().Device.Mute {
		t.Fatal("on")
	}
	f.do(t, "beep", model.ActOff)
	if !f.store.Get().Device.Mute {
		t.Fatal("off")
	}

	// With no alert on there is nothing to mute.
	cfg := f.store.Get()
	cfg.Device.Beep, cfg.Device.BeepDone, cfg.Device.Mute = false, false, false
	f.store.Set(cfg)
	if it := f.item(t, "beep"); it.Text != "未开启" || !it.Disabled {
		t.Fatalf("no alerts %+v", it)
	}
	cfg.Device.BeepDone = true
	f.store.Set(cfg)
	if it := f.item(t, "beep"); it.Text != "已开" || it.Disabled {
		t.Fatalf("done alert only %+v", it)
	}
}

func TestOldFirmware(t *testing.T) {
	f := newFixture(t)
	f.dev.err = errors.New("屏幕固件不支持调亮度，请先升级固件")
	pg, _ := f.p.Page()
	if pg.Error != f.dev.err.Error() {
		t.Fatalf("error %q", pg.Error)
	}
	for _, it := range pg.Items {
		if it.Disabled != (it.ID != "beep") {
			t.Errorf("%s disabled %v", it.ID, it.Disabled)
		}
	}
	if err := f.p.Do(context.Background(), "bright", model.ActInc); err == nil {
		t.Fatal("brightness changed for firmware that cannot take it")
	}
	if f.store.Get().Device.Brightness != 80 {
		t.Fatal("saved anyway")
	}
	f.do(t, "beep", model.ActOff) // the alerts are go-dock's own
}
