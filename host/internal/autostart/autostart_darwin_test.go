package autostart

import (
	"os"
	"strings"
	"testing"
)

func TestLaunchAgent(t *testing.T) {
	dir := t.TempDir()
	home = func() (string, error) { return dir, nil }
	defer func() { home = os.UserHomeDir }()
	exe := "/Applications/My Tools/go-dock&co"

	if st := Get(exe); !st.Supported || st.Enabled || st.Method != "launchd" {
		t.Fatalf("before = %+v", st)
	}
	if err := Enable(exe); err != nil {
		t.Fatal(err)
	}
	st := Get(exe)
	if !st.Enabled || st.Stale || !strings.HasSuffix(st.Path, "LaunchAgents/"+label+".plist") {
		t.Fatalf("after enable = %+v", st)
	}
	data, _ := os.ReadFile(st.Path)
	if !strings.Contains(string(data), "<string>/Applications/My Tools/go-dock&amp;co</string>") ||
		!strings.Contains(string(data), "<string>--background</string>") {
		t.Fatalf("plist = %s", data)
	}
	if st := Get("/other/go-dock"); !st.Stale {
		t.Fatal("moved exe not stale")
	}
	if err := Disable(); err != nil {
		t.Fatal(err)
	}
	if err := Disable(); err != nil {
		t.Fatal("second disable:", err)
	}
	if st := Get(exe); st.Enabled {
		t.Fatalf("after disable = %+v", st)
	}
}
