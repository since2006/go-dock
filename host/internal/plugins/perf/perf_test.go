package perf

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/sysmon"
)

const giB = 1 << 30

// mac is a reading on a Mac: everything known.
var mac = sysmon.Sample{
	CPU: 37.6, GPU: 57,
	Mem:  sysmon.Memory{Usage: sysmon.Usage{Used: 20 * giB, Total: 36 * giB}, Pressure: 1},
	Disk: sysmon.Usage{Used: 900 * giB, Total: 1000 * giB},
	Fans: []float64{0, 0},
	Temp: 45.6,
}

func enabled() *Plugin {
	p := New(nil, nil)
	p.SetOptions(Options{Enabled: true})
	return p
}

func ptr(v int) *int { return &v }

func ids(rs []model.Reading) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}

func reading(t *testing.T, rs []model.Reading, id string) model.Reading {
	t.Helper()
	for _, r := range rs {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no %s in %v", id, ids(rs))
	return model.Reading{}
}

// A Mac reports everything: six readings in line order, a bar on each but
// the fan and the temperature.
func TestReadings(t *testing.T) {
	p := enabled()
	if rs := p.Readings(); rs != nil {
		t.Fatalf("readings before the first sample: %+v", rs)
	}
	p.add(mac)
	rs := p.Readings()
	want := []model.Reading{
		{ID: "cpu", Icon: model.IconCPU, Label: "CPU", Text: "38", Unit: "%", Pct: ptr(38)},
		{ID: "gpu", Icon: model.IconGPU, Label: "GPU", Text: "57", Unit: "%", Pct: ptr(57)},
		{ID: "mem", Icon: model.IconMemory, Label: "内存", Text: "56", Unit: "%", Pct: ptr(56)},
		{ID: "fan", Icon: model.IconRotor, Label: "风扇", Text: "停转"},
		{ID: "temp", Icon: model.IconTemp, Label: "温度", Text: "46", Unit: "°C"},
		{ID: "disk", Icon: model.IconDisk, Label: "磁盘", Text: "90", Unit: "%", Pct: ptr(90), Level: model.LevelWarn},
	}
	if !reflect.DeepEqual(rs, want) {
		t.Fatalf("readings\n got %+v\nwant %+v", rs, want)
	}

	s := mac
	s.Fans = []float64{2150, 3449}
	p.add(s)
	if f := reading(t, p.Readings(), "fan"); f.Text != "3449" || f.Unit != "rpm" || f.Pct != nil {
		t.Fatalf("fan %+v", f)
	}
}

// What a computer cannot report is left out.
func TestReadingsMissing(t *testing.T) {
	p := enabled()
	s := mac
	s.GPU, s.Fans, s.Temp, s.Disk = -1, nil, 0, sysmon.Usage{}
	p.add(s)
	if got, want := ids(p.Readings()), []string{"cpu", "mem"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("readings %v, want %v", got, want)
	}
}

// On a Mac the memory's colour follows the memory pressure; elsewhere the
// percent decides.
func TestMemory(t *testing.T) {
	cases := []struct {
		name  string
		mem   sysmon.Memory
		level string
	}{
		{"mac, busy but fine", sysmon.Memory{Usage: sysmon.Usage{Used: 34 * giB, Total: 36 * giB}, Pressure: 1}, ""},
		{"mac, warn", sysmon.Memory{Usage: sysmon.Usage{Used: 30 * giB, Total: 36 * giB}, Pressure: 2}, model.LevelWarn},
		{"mac, critical", sysmon.Memory{Usage: sysmon.Usage{Used: 35 * giB, Total: 36 * giB}, Pressure: 4}, model.LevelCrit},
		{"linux, fine", sysmon.Memory{Usage: sysmon.Usage{Used: 16 * giB, Total: 32 * giB}}, ""},
		{"linux, full", sysmon.Memory{Usage: sysmon.Usage{Used: 31 * giB, Total: 32 * giB}}, model.LevelCrit},
	}
	for _, c := range cases {
		p := enabled()
		s := mac
		s.Mem = c.mem
		p.add(s)
		if got := reading(t, p.Readings(), "mem").Level; got != c.level {
			t.Errorf("%s: level %q, want %q", c.name, got, c.level)
		}
	}
}

func TestLevels(t *testing.T) {
	p := enabled()
	s := mac
	s.CPU, s.GPU, s.Disk = 95, 75, sysmon.Usage{Used: 50 * giB, Total: 100 * giB}
	p.add(s)
	rs := p.Readings()
	for id, want := range map[string]string{"cpu": model.LevelCrit, "gpu": model.LevelWarn, "disk": "", "temp": "", "fan": ""} {
		if got := reading(t, rs, id).Level; got != want {
			t.Errorf("%s level = %q, want %q", id, got, want)
		}
	}
	for c, want := range map[float64]string{84.4: "", 85: model.LevelWarn, 94.9: model.LevelWarn, 95: model.LevelCrit} {
		s.Temp = c
		p.add(s)
		if got := reading(t, p.Readings(), "temp").Level; got != want {
			t.Errorf("temp %v level = %q, want %q", c, got, want)
		}
	}
}

func TestDisabled(t *testing.T) {
	p := enabled()
	p.add(mac)
	p.SetOptions(Options{})
	if rs := p.Readings(); rs != nil {
		t.Fatalf("readings while off: %+v", rs)
	}
	p.add(mac) // a reading that finishes after it was turned off
	if rs := p.Readings(); rs != nil {
		t.Fatalf("late reading kept: %+v", rs)
	}
	// Turned on again it starts over.
	p.SetOptions(Options{Enabled: true})
	if rs := p.Readings(); rs != nil {
		t.Fatalf("old readings kept: %+v", rs)
	}
}

type fakeReader struct{ reads chan time.Time }

func (f *fakeReader) Read(context.Context) sysmon.Sample {
	f.reads <- time.Now()
	return mac
}

// Run starts reading when it is turned on; the first reading only
// starts the CPU's count, and the next comes a whole Every later.
func TestRun(t *testing.T) {
	r := &fakeReader{reads: make(chan time.Time, 8)}
	changed := make(chan struct{}, 8)
	p := New(func() Reader { return r }, func() { changed <- struct{}{} })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	select {
	case <-r.reads:
		t.Fatal("read while off")
	case <-time.After(50 * time.Millisecond):
	}
	p.SetOptions(Options{Enabled: true})
	var first time.Time
	select {
	case first = <-r.reads:
	case <-time.After(time.Second):
		t.Fatal("no reading after it was turned on")
	}
	if rs := p.Readings(); len(rs) != 0 {
		t.Fatal("the first reading was shown")
	}
	select {
	case second := <-r.reads:
		if gap := second.Sub(first); gap < Every*9/10 {
			t.Fatalf("second reading %v after the first", gap)
		}
	case <-time.After(Every + time.Second):
		t.Fatal("no second reading")
	}
	deadline := time.After(time.Second)
	for {
		if rs := p.Readings(); len(rs) == 6 {
			break
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatal("second reading not shown")
		}
	}
}
