package autostart

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func fakeLinux(t *testing.T, systemd bool) (string, *[]string) {
	dir := t.TempDir()
	var calls []string
	oldDir, oldCtl, oldHas := configDir, systemctl, hasSystemd
	configDir = func() (string, error) { return dir, nil }
	systemctl = func(args ...string) error { calls = append(calls, strings.Join(args, " ")); return nil }
	hasSystemd = func() bool { return systemd }
	t.Cleanup(func() { configDir, systemctl, hasSystemd = oldDir, oldCtl, oldHas })
	return dir, &calls
}

func TestSystemdUnit(t *testing.T) {
	dir, calls := fakeLinux(t, true)
	exe := "/opt/my tools/go-dock"
	if err := Enable(exe); err != nil {
		t.Fatal(err)
	}
	st := Get(exe)
	if !st.Enabled || st.Stale || st.Method != "systemd" {
		t.Fatalf("status = %+v", st)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "systemd", "user", unitName))
	if !strings.Contains(string(data), `ExecStart="/opt/my tools/go-dock" run --background`) {
		t.Fatalf("unit = %s", data)
	}
	if !slices.Contains(*calls, "enable go-dock.service") || slices.Contains(*calls, "start go-dock.service") {
		t.Fatalf("systemctl calls = %v", *calls)
	}
	if st := Get("/usr/bin/go-dock"); !st.Stale {
		t.Fatal("moved exe not stale")
	}
	if err := Disable(); err != nil {
		t.Fatal(err)
	}
	if st := Get(exe); st.Enabled || !slices.Contains(*calls, "disable go-dock.service") {
		t.Fatalf("after disable = %+v, calls %v", st, *calls)
	}
}

func TestXDGFallback(t *testing.T) {
	dir, calls := fakeLinux(t, false)
	exe := "/home/me/bin/go-dock"
	if st := Get(exe); st.Method != "xdg" || st.Enabled {
		t.Fatalf("before = %+v", st)
	}
	if err := Enable(exe); err != nil {
		t.Fatal(err)
	}
	if st := Get(exe); !st.Enabled || st.Stale || st.Method != "xdg" {
		t.Fatalf("status = %+v", st)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "autostart", "go-dock.desktop"))
	if !strings.Contains(string(data), "Exec=/home/me/bin/go-dock run --background") {
		t.Fatalf("desktop = %s", data)
	}
	if len(*calls) != 0 {
		t.Fatalf("systemctl used without systemd: %v", *calls)
	}
	if err := Disable(); err != nil || Get(exe).Enabled {
		t.Fatalf("disable: %v", err)
	}
}
