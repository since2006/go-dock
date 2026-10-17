package config

import (
	"os"
	"path/filepath"
	"strings"
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
	if err != nil || !strings.Contains(string(b), `"done_ttl": "30m0s"`) {
		t.Fatalf("config not written: %v\n%s", err, b)
	}
}

func TestPartialFileKeepsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"display":{"done_ttl":"5m"}}`), 0o600)
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c := s.Get()
	if c.Display.DoneTTL.D() != 5*time.Minute || c.Display.MaxSessions != 12 || !c.Claude.Enabled {
		t.Fatalf("merge wrong: %+v", c)
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
