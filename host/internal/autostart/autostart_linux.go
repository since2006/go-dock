package autostart

import (
	"bufio"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/since2006/go-dock/host/internal/config"
)

const unitName = "go-dock.service"

// Replaced in tests.
var (
	configDir = func() (string, error) {
		if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
			return d, nil
		}
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(h, ".config"), nil
	}
	systemctl = func(args ...string) error {
		return exec.Command("systemctl", append([]string{"--user"}, args...)...).Run()
	}
	hasSystemd = func() bool { return systemctl("show-environment") == nil }
)

func unitPath() (string, error) {
	d, err := configDir()
	return filepath.Join(d, "systemd", "user", unitName), err
}

func desktopPath() (string, error) {
	d, err := configDir()
	return filepath.Join(d, "autostart", "go-dock.desktop"), err
}

// quote quotes a path for systemd ExecStart and desktop Exec lines.
func quote(s string) string {
	if !strings.ContainsAny(s, " \t\"\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func execLine(exe string) string { return quote(exe) + " " + strings.Join(Args, " ") }

func unitFile(exe string) []byte {
	return []byte(`[Unit]
Description=go-dock (Claude Code / Codex status screen)

[Service]
ExecStart=` + execLine(exe) + `
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`)
}

func desktopFile(exe string) []byte {
	return []byte(`[Desktop Entry]
Type=Application
Name=go-dock
Comment=Claude Code / Codex status screen
Exec=` + execLine(exe) + `
Terminal=false
NoDisplay=true
X-GNOME-Autostart-enabled=true
`)
}

// execOf returns the executable from the ExecStart= or Exec= line.
func execOf(data []byte, key string) string {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), key+"=")
		if !ok {
			continue
		}
		if strings.HasPrefix(line, `"`) {
			if end := strings.Index(line[1:], `"`); end >= 0 {
				return strings.NewReplacer(`\\`, `\`, `\"`, `"`).Replace(line[1 : end+1])
			}
		}
		if f := strings.Fields(line); len(f) > 0 {
			return f[0]
		}
	}
	return ""
}

func get(exe string) (Status, error) {
	st := Status{Supported: true}
	for _, c := range []struct {
		method string
		path   func() (string, error)
		key    string
	}{{"systemd", unitPath, "ExecStart"}, {"xdg", desktopPath, "Exec"}} {
		path, err := c.path()
		if err != nil {
			return st, err
		}
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return st, err
		}
		st.Enabled, st.Method, st.Path = true, c.method, path
		st.Stale = execOf(data, c.key) != exe
		return st, nil
	}
	if hasSystemd() {
		st.Method = "systemd"
	} else {
		st.Method = "xdg"
	}
	return st, nil
}

// enable prefers a systemd user unit, which also works without a desktop
// session, and falls back to XDG autostart. It does not start the unit now,
// since this instance is already running.
func enable(exe string) error {
	if err := disable(); err != nil {
		return err
	}
	if hasSystemd() {
		path, err := unitPath()
		if err != nil {
			return err
		}
		if err := config.WriteFileAtomic(path, unitFile(exe), 0o644); err != nil {
			return err
		}
		if err := systemctl("daemon-reload"); err != nil {
			return err
		}
		return systemctl("enable", unitName)
	}
	path, err := desktopPath()
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, desktopFile(exe), 0o644)
}

func disable() error {
	if path, err := unitPath(); err == nil {
		if _, err := os.Stat(path); err == nil {
			_ = systemctl("disable", unitName)
			if err := os.Remove(path); err != nil {
				return err
			}
			_ = systemctl("daemon-reload")
		}
	}
	path, err := desktopPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
