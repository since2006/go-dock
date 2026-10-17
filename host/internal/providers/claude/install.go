package claude

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/since2006/go-dock/host/internal/providers"
)

// hookEvents are the Claude Code hook events go-dock listens to.
var hookEvents = []string{
	"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse",
	"PermissionRequest", "Notification", "Stop", "StopFailure", "SessionEnd",
}

func settingsPath(home string) string { return filepath.Join(home, "settings.json") }

// hookHandler uses the exec form (command + args) so no shell is involved
// on any platform. Events other than SessionEnd run async so Claude never
// waits on go-dock; SessionEnd must finish before the process exits.
func hookHandler(t providers.Target, event string) map[string]any {
	h := map[string]any{
		"type":    "command",
		"command": t.Exe,
		"args":    []any{"hook", "claude", "--addr", t.Addr},
		"timeout": 10,
	}
	if event == "SessionEnd" {
		h["timeout"] = 2
	} else {
		h["async"] = true
	}
	return h
}

func isOurHook(h map[string]any) bool {
	cmd, _ := h["command"].(string)
	if !providers.IsGoDockExe(firstField(cmd)) {
		return false
	}
	if args, ok := h["args"].([]any); ok {
		return len(args) >= 2 && args[0] == "hook" && args[1] == "claude"
	}
	return strings.Contains(cmd, " hook claude")
}

func hookMatches(h map[string]any, want map[string]any) bool {
	return h["command"] == want["command"] && slices.Equal(anyStrings(h["args"]), anyStrings(want["args"]))
}

func anyStrings(v any) []string {
	a, _ := v.([]any)
	out := make([]string, 0, len(a))
	for _, e := range a {
		s, _ := e.(string)
		out = append(out, s)
	}
	return out
}

func firstField(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}
	if q := cmd[0]; q == '"' || q == '\'' {
		if i := strings.IndexByte(cmd[1:], q); i >= 0 {
			return cmd[1 : i+1]
		}
	}
	if strings.HasPrefix(cmd, "& ") {
		return firstField(cmd[2:])
	}
	if i := strings.IndexByte(cmd, ' '); i >= 0 {
		return cmd[:i]
	}
	return cmd
}

// HooksStatus reports whether every hook event points at t.
func HooksStatus(home string, t providers.Target) providers.Status {
	st := providers.Status{Name: "claude-hooks", Title: "Claude Code hooks", File: settingsPath(home)}
	f, err := providers.ReadJSONFile(st.File)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	found, current := 0, 0
	for _, ev := range hookEvents {
		ours := providers.FindHooks(providers.Groups(f.Get("hooks."+ev)), isOurHook)
		if len(ours) > 0 {
			found++
			if hookMatches(ours[0], hookHandler(t, ev)) {
				current++
			}
		}
	}
	st.Installed = found > 0
	st.Stale = found > 0 && (current < len(hookEvents) || found < len(hookEvents))
	if st.Stale {
		st.Note = "已安装的 hooks 与当前程序路径或端口不一致，请重新安装"
	}
	return st
}

// InstallHooks adds or refreshes go-dock's hooks in settings.json.
func InstallHooks(home string, t providers.Target) error {
	f, err := providers.ReadJSONFile(settingsPath(home))
	if err != nil {
		return err
	}
	for _, ev := range hookEvents {
		groups := providers.MergeHook(providers.Groups(f.Get("hooks."+ev)), hookHandler(t, ev), isOurHook)
		if err := f.Set("hooks."+ev, groups); err != nil {
			return err
		}
	}
	return f.Save()
}

// UninstallHooks removes go-dock's hooks from every event.
func UninstallHooks(home string) error {
	f, err := providers.ReadJSONFile(settingsPath(home))
	if err != nil || !f.Exists {
		return err
	}
	hooks := f.Get("hooks")
	if !hooks.IsObject() {
		return nil
	}
	for ev := range hooks.Map() {
		path := "hooks." + ev
		before := providers.Groups(f.Get(path))
		if len(providers.FindHooks(before, isOurHook)) == 0 {
			continue
		}
		after := providers.RemoveHooks(before, isOurHook)
		if len(after) == 0 {
			err = f.Delete(path)
		} else {
			err = f.Set(path, after)
		}
		if err != nil {
			return err
		}
	}
	if h := f.Get("hooks"); h.IsObject() && len(h.Map()) == 0 {
		if err := f.Delete("hooks"); err != nil {
			return err
		}
	}
	return f.Save()
}

// statuslineCommand wraps chain, the user's previous statusLine command.
// The chain is base64url encoded so it survives any shell unchanged.
func statuslineCommand(t providers.Target, chain string) string {
	args := []string{"statusline", "--addr", t.Addr}
	if chain != "" {
		args = append(args, "--chain", base64.RawURLEncoding.EncodeToString([]byte(chain)))
	}
	return providers.PortableCommand(t.Exe, args...)
}

func isOurStatusline(cmd string) bool {
	return providers.IsGoDockExe(firstField(cmd)) && strings.Contains(cmd, " statusline")
}

// ChainFromCommand extracts the wrapped command from our statusline command.
func ChainFromCommand(cmd string) (string, error) {
	fields := strings.Fields(cmd)
	for i, f := range fields {
		if f == "--chain" && i+1 < len(fields) {
			b, err := base64.RawURLEncoding.DecodeString(fields[i+1])
			if err != nil {
				return "", fmt.Errorf("解析 --chain: %w", err)
			}
			return string(b), nil
		}
	}
	return "", nil
}

func StatuslineStatus(home string, t providers.Target) providers.Status {
	st := providers.Status{Name: "claude-statusline", Title: "Claude Code 状态栏（额度）", File: settingsPath(home)}
	f, err := providers.ReadJSONFile(st.File)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	cmd := f.Get("statusLine.command").String()
	if !isOurStatusline(cmd) {
		if cmd != "" {
			st.Note = "安装后会包装现有状态栏命令，显示效果不变"
		}
		return st
	}
	st.Installed = true
	chain, err := ChainFromCommand(cmd)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	if cmd != statuslineCommand(t, chain) {
		st.Stale = true
		st.Note = "状态栏命令与当前程序路径或端口不一致，请重新安装"
	} else if chain != "" {
		st.Note = "已包装原状态栏命令：" + chain
	}
	return st
}

// InstallStatusline points statusLine at go-dock, wrapping any existing
// command so the user's status line keeps rendering.
func InstallStatusline(home string, t providers.Target) error {
	f, err := providers.ReadJSONFile(settingsPath(home))
	if err != nil {
		return err
	}
	cur := f.Get("statusLine.command").String()
	chain := cur
	if isOurStatusline(cur) {
		if chain, err = ChainFromCommand(cur); err != nil {
			return err
		}
	}
	if f.Get("statusLine.type").String() == "" {
		if err := f.Set("statusLine.type", "command"); err != nil {
			return err
		}
	}
	if err := f.Set("statusLine.command", statuslineCommand(t, chain)); err != nil {
		return err
	}
	return f.Save()
}

// UninstallStatusline restores the wrapped command, or removes statusLine
// when there was none.
func UninstallStatusline(home string) error {
	f, err := providers.ReadJSONFile(settingsPath(home))
	if err != nil || !f.Exists {
		return err
	}
	cur := f.Get("statusLine.command").String()
	if !isOurStatusline(cur) {
		return nil
	}
	chain, err := ChainFromCommand(cur)
	if err != nil {
		return err
	}
	if chain == "" {
		err = f.Delete("statusLine")
	} else {
		err = f.Set("statusLine.command", chain)
	}
	if err != nil {
		return err
	}
	return f.Save()
}
