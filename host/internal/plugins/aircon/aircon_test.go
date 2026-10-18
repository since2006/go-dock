package aircon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/since2006/go-dock/host/internal/ir"
	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/plugins"
)

type fakeRemote struct {
	mu   sync.Mutex
	err  error // the screen cannot send: why
	sent []ir.Gree
}

func (r *fakeRemote) SendIR(hz int, raw []int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	g, err := ir.DecodeGree(raw)
	if hz != ir.Carrier || err != nil {
		return errors.New("not a Gree frame")
	}
	r.sent = append(r.sent, g)
	return nil
}

func (r *fakeRemote) IRReady() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *fakeRemote) frames() []ir.Gree {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ir.Gree(nil), r.sent...)
}

type fixture struct {
	p       *Plugin
	r       *fakeRemote
	path    string
	logs    *bytes.Buffer
	changes atomic.Int32
}

func newFixture(t *testing.T, o Options) *fixture {
	t.Helper()
	f := &fixture{r: &fakeRemote{}, path: filepath.Join(t.TempDir(), "aircon.json"), logs: &bytes.Buffer{}}
	log := slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.p = New(f.r, f.path, log, func() { f.changes.Add(1) })
	o.Enabled = true
	f.p.SetOptions(o)
	return f
}

func (f *fixture) do(t *testing.T, item, act string) {
	t.Helper()
	if err := f.p.Do(context.Background(), item, act); err != nil {
		t.Fatalf("%s %s: %v", item, act, err)
	}
}

func (f *fixture) page(t *testing.T) model.Page {
	t.Helper()
	pg, ok := f.p.Page()
	if !ok {
		t.Fatal("page hidden")
	}
	return pg
}

func (f *fixture) item(t *testing.T, id string) model.PageItem {
	t.Helper()
	for _, it := range f.page(t).Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("no item %q", id)
	return model.PageItem{}
}

func TestPowerOn(t *testing.T) {
	f := newFixture(t, Options{})
	pg := f.page(t)
	if pg.Title != "空调" || pg.Summary != "已关机" || pg.Error != "" || len(pg.Items) != 4 {
		t.Fatalf("page %+v", pg)
	}
	if it := f.item(t, "power"); it.Kind != model.KindSwitch || it.Icon != model.IconPower || it.Text != "已关" || *it.On {
		t.Fatalf("power %+v", it)
	}

	f.do(t, "power", model.ActToggle)
	sent := f.r.frames()
	// A fresh remote's settings, switched on: auto, 25°C, auto fan.
	if len(sent) != 1 || sent[0] != (ir.Gree{0x08, 0x09, 0x60, 0x50, 0x00, 0x20, 0x00, 0xD0}) {
		t.Fatalf("sent %v", sent)
	}
	if pg := f.page(t); pg.Summary != "自动 25°C · 自动风" || pg.Items[0].Text != "已开" || !*pg.Items[0].On {
		t.Fatalf("after power on %+v", pg)
	}
	if !strings.Contains(f.logs.String(), "已发送空调红外") {
		t.Fatal("not logged")
	}
}

func TestModesAndRules(t *testing.T) {
	f := newFixture(t, Options{Name: "会议室"})
	f.do(t, "power", model.ActOn)
	if it := f.item(t, "temp"); !it.Disabled || it.Text != "25°C" {
		t.Fatalf("auto holds the temperature: %+v", it)
	}
	f.do(t, "mode", model.ActNext)
	if it := f.item(t, "mode"); it.Text != "制冷" || it.Icon != model.IconCool || it.Kind != model.KindSelect {
		t.Fatalf("mode %+v", it)
	}
	f.do(t, "temp", model.ActInc)
	if it := f.item(t, "temp"); it.Disabled || it.Text != "26°C" || it.Kind != model.KindStepper {
		t.Fatalf("temp %+v", it)
	}
	f.do(t, "fan", model.ActNext)
	f.do(t, "fan", model.ActNext)
	if pg := f.page(t); pg.Title != "会议室" || pg.Summary != "制冷 26°C · 中风" {
		t.Fatalf("page %+v", pg)
	}

	f.do(t, "mode", model.ActNext) // heat
	f.do(t, "mode", model.ActNext) // dry holds the fan low
	if it := f.item(t, "fan"); !it.Disabled || it.Text != "低" {
		t.Fatalf("dry fan %+v", it)
	}
	n := len(f.r.frames())
	f.do(t, "fan", model.ActNext)
	if len(f.r.frames()) != n {
		t.Fatal("sent a frame that changes nothing")
	}
	f.do(t, "mode", model.ActNext) // fan only
	if pg := f.page(t); pg.Summary != "送风 · 低风" || pg.Items[2].Icon != model.IconFan {
		t.Fatalf("fan mode %+v", pg)
	}
	f.do(t, "mode", model.ActNext) // auto
	f.do(t, "mode", model.ActNext) // and round to cool
	last := f.r.frames()[len(f.r.frames())-1]
	if last.Mode() != ir.ModeCool || last.Temp() != ir.GreeAutoTemp || !last.Power() {
		t.Fatalf("last frame %v", last)
	}
}

func TestTempLimits(t *testing.T) {
	f := newFixture(t, Options{})
	f.do(t, "power", model.ActOn)
	f.do(t, "mode", model.ActNext)
	for range 10 {
		f.do(t, "temp", model.ActInc)
	}
	n := len(f.r.frames())
	f.do(t, "temp", model.ActInc)
	if it := f.item(t, "temp"); it.Text != "30°C" || !it.AtMax || it.AtMin || len(f.r.frames()) != n {
		t.Fatalf("at 30: %+v, %d frames", it, len(f.r.frames()))
	}
	for range 20 {
		f.do(t, "temp", model.ActDec)
	}
	if it := f.item(t, "temp"); it.Text != "16°C" || !it.AtMin {
		t.Fatalf("at 16: %+v", it)
	}
}

func TestOffKeepsChanges(t *testing.T) {
	f := newFixture(t, Options{})
	f.do(t, "mode", model.ActNext)
	f.do(t, "temp", model.ActDec)
	f.do(t, "temp", model.ActDec)
	if n := len(f.r.frames()); n != 0 {
		t.Fatalf("sent %d frames while off", n)
	}
	f.do(t, "power", model.ActToggle)
	sent := f.r.frames()
	if len(sent) != 1 || sent[0].Mode() != ir.ModeCool || sent[0].Temp() != 23 || !sent[0].Power() {
		t.Fatalf("power on sent %v", sent)
	}
	f.do(t, "power", model.ActToggle)
	if sent := f.r.frames(); len(sent) != 2 || sent[1].Power() {
		t.Fatalf("power off sent %v", sent)
	}
}

func TestScreenCannotSend(t *testing.T) {
	f := newFixture(t, Options{})
	f.r.err = errors.New("屏幕固件不支持红外，请先升级固件")
	pg := f.page(t)
	if pg.Error != f.r.err.Error() {
		t.Fatalf("error %q", pg.Error)
	}
	for _, it := range pg.Items {
		if !it.Disabled {
			t.Fatalf("%s not greyed", it.ID)
		}
	}
	if err := f.p.Do(context.Background(), "power", model.ActOn); err == nil {
		t.Fatal("no error")
	}
	if f.item(t, "power").Text != "已关" {
		t.Fatal("state changed though nothing was sent")
	}
}

func TestBadTaps(t *testing.T) {
	f := newFixture(t, Options{})
	for _, c := range []struct {
		item, act string
		want      error
	}{
		{"temp", model.ActToggle, plugins.ErrBadAct},
		{"mode", model.ActInc, plugins.ErrBadAct},
		{"power", model.ActNext, plugins.ErrBadAct},
		{"swing", model.ActNext, plugins.ErrNotFound},
	} {
		if err := f.p.Do(context.Background(), c.item, c.act); !errors.Is(err, c.want) {
			t.Errorf("%s %s: %v", c.item, c.act, err)
		}
	}
	f.p.SetOptions(Options{})
	if _, ok := f.p.Page(); ok {
		t.Fatal("disabled page shown")
	}
	if err := f.p.Do(context.Background(), "power", model.ActOn); !errors.Is(err, plugins.ErrNotFound) {
		t.Fatalf("disabled: %v", err)
	}
}

func TestRemoteKeepsPageInStep(t *testing.T) {
	f := newFixture(t, Options{Model: ir.GreeYBOFB})
	// The unit's own remote: heat, 28°C, high fan, auto swing.
	g := ir.GreeDefault()
	g.SetPower(true)
	g.SetMode(ir.ModeHeat)
	g.SetTemp(28)
	g.SetFan(ir.FanHigh)
	g[0] |= 0x40
	before := f.changes.Load()
	f.p.Received(ir.GreeRaw(g.Frame(ir.GreeYAW1F)))
	if pg := f.page(t); pg.Summary != "制热 28°C · 高风" || f.changes.Load() == before {
		t.Fatalf("page %+v", pg)
	}
	f.p.Received(ir.GreeRaw(g.Frame(ir.GreeYAW1F))) // the same again
	if n := strings.Count(f.logs.String(), "遥控器像是另一种格力型号"); n != 1 {
		t.Fatalf("model hint %d times", n)
	}
	if n := strings.Count(f.logs.String(), "收到格力遥控器信号"); n != 1 {
		t.Fatalf("logged the same state %d times", n)
	}

	// The next tap keeps what the remote set and go-dock does not.
	f.do(t, "temp", model.ActDec)
	sent := f.r.frames()
	if len(sent) != 1 || sent[0][0]&0x40 == 0 || sent[0].Temp() != 27 || sent[0][2]&0x40 != 0 {
		t.Fatalf("sent %v", sent)
	}

	f.p.Received([]int{4500, 4500, 560, 1690}) // some other remote
	if f.page(t).Summary != "制热 27°C · 高风" {
		t.Fatal("a foreign frame changed the state")
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	f := newFixture(t, Options{})
	f.do(t, "power", model.ActOn)
	f.do(t, "mode", model.ActNext)
	fi, err := os.Stat(f.path)
	if err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) { // Windows has no such bits
		t.Fatalf("state file: %v %v", fi, err)
	}
	again := New(&fakeRemote{}, f.path, slog.New(slog.NewTextHandler(f.logs, nil)), nil)
	again.SetOptions(Options{Enabled: true})
	if pg, _ := again.Page(); pg.Summary != "制冷 25°C · 自动风" {
		t.Fatalf("restored %+v", pg)
	}

	os.WriteFile(f.path, []byte(`{"gree":"zz"}`), 0o600)
	broken := New(&fakeRemote{}, f.path, slog.New(slog.NewTextHandler(f.logs, nil)), nil)
	broken.SetOptions(Options{Enabled: true})
	if pg, _ := broken.Page(); pg.Summary != "已关机" || !strings.Contains(f.logs.String(), "读不懂") {
		t.Fatalf("broken file: %+v", pg)
	}
}
