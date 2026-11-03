package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoadCreatesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Get().Web.Listen != DefaultListen {
		t.Fatalf("defaults not applied: %+v", s.Get().Web)
	}
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), `"done_ttl": "30m"`) {
		t.Fatalf("config not written: %v\n%s", err, b)
	}
}

func TestPartialFileKeepsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// Written by a version without the beep settings: they start on.
	os.WriteFile(path, []byte(`{"device":{"enabled":true,"port":"COM3"},"display":{"done_ttl":"5m"}}`), 0o600)
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c := s.Get()
	if c.Display.DoneTTL.D() != 5*time.Minute || c.Display.MaxSessions != 12 || !c.Claude.Enabled {
		t.Fatalf("merge wrong: %+v", c)
	}
	if c.Device.Port != "COM3" || !c.Device.Beep || c.Device.BeepSound != "chime" {
		t.Fatalf("device = %+v", c.Device)
	}
}

func TestSetValidatesAndNotifies(t *testing.T) {
	s, _ := Load(filepath.Join(t.TempDir(), "config.json"))
	var got Config
	s.OnChange(func(_, cur Config) { got = cur })

	bad := s.Get()
	bad.Log.Level = "loud"
	if err := s.Set(bad); err == nil {
		t.Fatal("invalid config accepted")
	}
	good := s.Get()
	good.Device.Port = "COM3"
	if err := s.Set(good); err != nil {
		t.Fatal(err)
	}
	if got.Device.Port != "COM3" {
		t.Fatal("listener not called")
	}
	reloaded, _ := Load(s.Path())
	if reloaded.Get().Device.Port != "COM3" {
		t.Fatal("config not persisted")
	}
}

func TestBadDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"display":{"done_ttl":"soon"}}`), 0o600)
	if _, err := Load(path); err == nil {
		t.Fatal("bad duration accepted")
	}
}

func TestDurationString(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Minute:           "30m",
		12 * time.Hour:             "12h",
		90 * time.Minute:           "1h30m",
		45 * time.Second:           "45s",
		time.Hour + 30*time.Second: "1h0m30s",
	}
	for d, want := range cases {
		if got := Duration(d).String(); got != want {
			t.Errorf("%v = %q, want %q", time.Duration(d), got, want)
		}
	}
}

func TestGetSharesNothing(t *testing.T) {
	s, _ := Load(filepath.Join(t.TempDir(), "config.json"))
	c := s.Get()
	c.Pricing = map[string]Price{"m": {Input: 1}}
	if err := s.Set(c); err != nil {
		t.Fatal(err)
	}

	// Decoding onto a copy, as POST /api/config does, leaves the store alone.
	cur := s.Get()
	if err := json.Unmarshal([]byte(`{"pricing":{"m":{"input":2}}}`), &cur); err != nil {
		t.Fatal(err)
	}
	cur.Pricing["m"] = Price{Input: 3}
	if stored := s.Get(); stored.Pricing["m"].Input != 1 {
		t.Fatalf("store changed: %+v", stored.Pricing)
	}
}

func TestValidateBrightness(t *testing.T) {
	c := Default()
	if d := c.Device; d.Brightness != 80 || d.DimBrightness != 20 || d.DimAfter.D() != 5*time.Minute ||
		d.BeepDone || d.BeepDoneSound != "rise" || d.Mute || d.Volume != 100 {
		t.Fatalf("defaults %+v", d)
	}
	c.Device.DimBrightness, c.Device.DimAfter = 0, 0 // off when idle; never dims
	c.Device.Volume = 5                              // the quietest
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(*DeviceConfig){
		"too dark":   func(d *DeviceConfig) { d.Brightness = 5 },
		"too bright": func(d *DeviceConfig) { d.Brightness = 101 },
		"dim below":  func(d *DeviceConfig) { d.DimBrightness = -1 },
		"too soon":   func(d *DeviceConfig) { d.DimAfter = Duration(30 * time.Second) },
		"too late":   func(d *DeviceConfig) { d.DimAfter = Duration(25 * time.Hour) },
		"too quiet":  func(d *DeviceConfig) { d.Volume = 4 },
		"too loud":   func(d *DeviceConfig) { d.Volume = 101 },
	} {
		c := Default()
		f(&c.Device)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestUpdate(t *testing.T) {
	s, _ := Load(filepath.Join(t.TempDir(), "config.json"))
	var got Config
	s.OnChange(func(_, cur Config) { got = cur })
	if err := s.Update(func(c *Config) { c.Device.Brightness = 50 }); err != nil {
		t.Fatal(err)
	}
	if got.Device.Brightness != 50 || s.Get().Device.Brightness != 50 {
		t.Fatal("not applied")
	}
	if err := s.Update(func(c *Config) { c.Device.Brightness = 0 }); err == nil || s.Get().Device.Brightness != 50 {
		t.Fatalf("invalid change: %v", err)
	}

	// Changes made together all land.
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			s.Update(func(c *Config) { c.Display.MaxSessions++ })
		})
	}
	wg.Wait()
	if n := s.Get().Display.MaxSessions; n != 22 {
		t.Fatalf("max sessions %d", n)
	}
}

func TestValidateDeviceScreen(t *testing.T) {
	// No screen set up yet.
	c := Default()
	if c.Device.Addr != "" || c.Device.Code != "" {
		t.Fatalf("defaults %+v", c.Device)
	}
	ok := c.Clone()
	ok.Device.Addr, ok.Device.Code, ok.Device.Name = "192.168.1.50", "482916", "书房的 Mac"
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func(d *DeviceConfig){
		"code without address": func(d *DeviceConfig) { d.Addr = " " },
		"address without code": func(d *DeviceConfig) { d.Code = "" },
		"short code":           func(d *DeviceConfig) { d.Code = "12345" },
		"letter code":          func(d *DeviceConfig) { d.Code = "12345a" },
		"long name":            func(d *DeviceConfig) { d.Name = "一二三四五六七八九十甲乙丙" },
	} {
		bad := ok.Clone()
		f(&bad.Device)
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
