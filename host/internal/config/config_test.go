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

func TestBrokerURL(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.10":              "mqtt://192.168.1.10:1883",
		" broker.lan:1884 ":         "mqtt://broker.lan:1884",
		"mqtt://h":                  "mqtt://h:1883",
		"MQTTS://h":                 "mqtts://h:8883",
		"tcp://h:1/":                "tcp://h:1",
		"ws://h:9001/mqtt":          "ws://h:9001/mqtt",
		"wss://broker.example/mqtt": "wss://broker.example/mqtt",
		"mqtt://[::1]":              "mqtt://[::1]:1883",
	} {
		got, err := BrokerURL(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "http://h", "mqtt://u:p@h", "mqtt://", "mqtt://h/topic", "mqtt://h?x=1"} {
		if got, err := BrokerURL(bad); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
}

func TestValidateMQTTAndLights(t *testing.T) {
	ok := Default()
	ok.MQTT.Brokers = List[Broker]{
		{ID: "office", Name: "办公室", Enabled: true, URL: "broker.lan"},
		{ID: "home", URL: "mqtts://home.example"},
		{ID: "spare"}, // disabled without an address is fine
	}
	ok.Plugins.Lights = LightsConfig{Enabled: true, Lights: List[Light]{
		{ID: "main", Name: "主灯", CommandTopic: "office/main/set", StateTopic: "office/main/state"},
		{ID: "desk-2", Name: "台灯", Broker: "home", CommandTopic: "office/desk/set"},
	}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := Default().Validate(); err != nil {
		t.Fatalf("defaults: %v", err)
	}

	light := func(f func(l *Light)) Config {
		c := ok.Clone()
		f(&c.Plugins.Lights.Lights[0])
		return c
	}
	broker := func(f func(b *Broker)) Config {
		c := ok.Clone()
		f(&c.MQTT.Brokers[0])
		return c
	}
	for name, c := range map[string]Config{
		"enabled without url": broker(func(b *Broker) { b.URL = "" }),
		"bad url":             broker(func(b *Broker) { b.Enabled = false; b.URL = "http://x" }),
		"bad broker id":       broker(func(b *Broker) { b.ID = "Office" }),
		"duplicate broker":    broker(func(b *Broker) { b.ID = "home" }),
		"long client id":      broker(func(b *Broker) { b.ClientID = strings.Repeat("x", 65) }),
		"unknown broker":      light(func(l *Light) { l.Broker = "nope" }),
		"broker in use removed": func() Config {
			c := ok.Clone()
			c.MQTT.Brokers = c.MQTT.Brokers[:1] // desk-2 still uses home
			return c
		}(),
		"bad id":           light(func(l *Light) { l.ID = "Main Light" }),
		"duplicate id":     light(func(l *Light) { l.ID = "desk-2" }),
		"no name":          light(func(l *Light) { l.Name = " " }),
		"no command":       light(func(l *Light) { l.CommandTopic = "" }),
		"wildcard state":   light(func(l *Light) { l.StateTopic = "office/+/state" }),
		"wildcard command": light(func(l *Light) { l.CommandTopic = "office/#" }),
		"long payload":     light(func(l *Light) { l.PayloadOn = strings.Repeat("x", 300) }),
		"too many": func() Config {
			c := ok.Clone()
			for i := range MaxLights {
				c.Plugins.Lights.Lights = append(c.Plugins.Lights.Lights, Light{ID: "l" + string(rune('a'+i)), Name: "x", CommandTopic: "x"})
			}
			return c
		}(),
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidateAircon(t *testing.T) {
	c := Default()
	if c.Plugins.Aircon.Model != "yaw1f" || c.Plugins.Aircon.Name != "空调" {
		t.Fatalf("defaults %+v", c.Plugins.Aircon)
	}
	c.Plugins.Aircon = AirconConfig{Enabled: true, Name: "会议室空调", Model: "ybofb"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, a := range map[string]AirconConfig{
		"long name": {Name: "会议室的那台格力空调"},
		"model":     {Model: "yx1fsf"},
	} {
		c.Plugins.Aircon = a
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidatePerf(t *testing.T) {
	c := Default()
	if c.Plugins.Perf.Enabled || c.Plugins.Perf.Order != nil {
		t.Fatalf("defaults %+v", c.Plugins.Perf)
	}
	// Unknown ids are the page's to skip; only malformed or repeated ones
	// are refused.
	c.Plugins.Perf.Order = []string{"mem", "cpu", "later"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, order := range map[string][]string{
		"repeated":  {"mem", "mem"},
		"malformed": {"Mem!"},
		"empty id":  {""},
	} {
		c.Plugins.Perf.Order = order
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestGetSharesNothing(t *testing.T) {
	s, _ := Load(filepath.Join(t.TempDir(), "config.json"))
	c := s.Get()
	c.Pricing = map[string]Price{"m": {Input: 1}}
	c.Plugins.Lights.Lights = List[Light]{{ID: "a", Name: "a", CommandTopic: "a"}, {ID: "b", Name: "b", CommandTopic: "b"}}
	if err := s.Set(c); err != nil {
		t.Fatal(err)
	}

	// Decoding onto a copy, as POST /api/config does, leaves the store alone
	// and replaces the list instead of merging into its elements.
	cur := s.Get()
	if err := json.Unmarshal([]byte(`{"pricing":{"m":{"input":2}},"plugins":{"lights":{"lights":[{"id":"b","name":"b2"}]}}}`), &cur); err != nil {
		t.Fatal(err)
	}
	if got := cur.Plugins.Lights.Lights; len(got) != 1 || got[0] != (Light{ID: "b", Name: "b2"}) {
		t.Fatalf("decoded list %+v", got)
	}
	stored := s.Get()
	if stored.Pricing["m"].Input != 1 || len(stored.Plugins.Lights.Lights) != 2 || stored.Plugins.Lights.Lights[0].ID != "a" {
		t.Fatalf("store changed: %+v %+v", stored.Pricing, stored.Plugins.Lights.Lights)
	}
}

func TestLightsWrittenAsList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"plugins":{"lights":{"enabled":true,"lights":null}}}`), 0o600)
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set(s.Get()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"lights": []`) || !strings.Contains(string(b), `"brokers": []`) {
		t.Fatalf("written:\n%s", b)
	}
}

func TestBrokerFor(t *testing.T) {
	m := MQTTConfig{Brokers: List[Broker]{{ID: "office"}, {ID: "home", Name: " 家 "}}}
	if m.BrokerFor("") != "office" || m.BrokerFor("home") != "home" || (MQTTConfig{}).BrokerFor("") != "" {
		t.Fatal("BrokerFor")
	}
	if m.Brokers[0].Label() != "office" || m.Brokers[1].Label() != "家" {
		t.Fatal("Label")
	}
}

func TestValidateBrightness(t *testing.T) {
	c := Default()
	if d := c.Device; d.Brightness != 80 || d.DimBrightness != 20 || d.DimAfter.D() != 5*time.Minute || !d.SettingsPage ||
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
