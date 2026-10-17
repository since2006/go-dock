package autostart

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/since2006/go-dock/host/internal/config"
)

const label = "io.github.since2006.go-dock"

// home is replaced in tests.
var home = os.UserHomeDir

func plistPath() (string, error) {
	h, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "Library", "LaunchAgents", label+".plist"), nil
}

func plist(exe string) []byte {
	var b bytes.Buffer
	esc := func(s string) string {
		var e bytes.Buffer
		_ = xml.EscapeText(&e, []byte(s))
		return e.String()
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + label + `</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, a := range append([]string{exe}, Args...) {
		b.WriteString("    <string>" + esc(a) + "</string>\n")
	}
	b.WriteString(`  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>ProcessType</key>
  <string>Interactive</string>
  <key>LimitLoadToSessionType</key>
  <string>Aqua</string>
</dict>
</plist>
`)
	return b.Bytes()
}

// programPath reads the executable back from an installed plist.
func programPath(data []byte) string {
	// ProgramArguments is the only array in our plist.
	var v struct {
		Dict struct {
			Array struct {
				Strings []string `xml:"string"`
			} `xml:"array"`
		} `xml:"dict"`
	}
	if xml.Unmarshal(data, &v) != nil || len(v.Dict.Array.Strings) == 0 {
		return ""
	}
	return strings.TrimSpace(v.Dict.Array.Strings[0])
}

func get(exe string) (Status, error) {
	st := Status{Supported: true, Method: "launchd"}
	path, err := plistPath()
	if err != nil {
		return st, err
	}
	st.Path = path
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.Enabled = true
	st.Stale = programPath(data) != exe
	return st, nil
}

// enable only writes the plist: launchd loads LaunchAgents at login, and
// loading it now would start a second go-dock next to this one.
func enable(exe string) error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, plist(exe), 0o644)
}

func disable() error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
