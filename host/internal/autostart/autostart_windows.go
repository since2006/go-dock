package autostart

import (
	"errors"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// runKey is replaced in tests.
var runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

const valueName = "go-dock"

func command(exe string) string { return `"` + exe + `" ` + strings.Join(Args, " ") }

func exeOf(cmd string) string {
	if strings.HasPrefix(cmd, `"`) {
		if end := strings.Index(cmd[1:], `"`); end >= 0 {
			return cmd[1 : end+1]
		}
	}
	if f := strings.Fields(cmd); len(f) > 0 {
		return f[0]
	}
	return ""
}

func get(exe string) (Status, error) {
	st := Status{Supported: true, Method: "registry", Path: `HKCU\` + runKey + `\` + valueName}
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	defer k.Close()
	cmd, _, err := k.GetStringValue(valueName)
	if errors.Is(err, registry.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.Enabled = true
	st.Stale = !strings.EqualFold(exeOf(cmd), exe)
	return st, nil
}

func enable(exe string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(valueName, command(exe))
}

func disable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(valueName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}
