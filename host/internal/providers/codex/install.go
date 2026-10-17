package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/since2006/go-dock/host/internal/providers"
)

// hookEvents are the Codex hook events go-dock listens to.
var hookEvents = []string{
	"SessionStart", "UserPromptSubmit", "PreToolUse", "PermissionRequest",
	"PostToolUse", "Stop", "Interrupt", "SessionEnd",
}

const trustNote = "安装后需在 Codex 中运行 /hooks 信任这些 hooks 才会生效"

func hooksPath(home string) string { return filepath.Join(home, "hooks.json") }

// hookHandler builds our handler. Codex runs command through the user's
// shell (PowerShell on Windows), so the command is quoted for that shell.
// SessionEnd always runs synchronously in Codex; it gets a short timeout.
func hookHandler(t providers.Target, event string) map[string]any {
	h := map[string]any{
		"type":    "command",
		"command": providers.ShellCommand(t.Exe, "hook", "codex", "--addr", t.Addr),
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
	return providers.IsGoDockExe(firstField(cmd)) && strings.Contains(cmd, " hook codex")
}

// firstField returns the executable of a shell command line, handling
// quotes and PowerShell's "& 'path'" call form.
func firstField(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, "& ") {
		cmd = strings.TrimSpace(cmd[2:])
	}
	if cmd == "" {
		return ""
	}
	if q := cmd[0]; q == '"' || q == '\'' {
		if i := strings.IndexByte(cmd[1:], q); i >= 0 {
			return cmd[1 : i+1]
		}
	}
	if i := strings.IndexByte(cmd, ' '); i >= 0 {
		return cmd[:i]
	}
	return cmd
}

func checkHome(home string) error {
	if fi, err := os.Stat(home); err != nil || !fi.IsDir() {
		return fmt.Errorf("未找到 Codex 目录 %s，请先安装并运行一次 Codex", home)
	}
	return nil
}

// HooksStatus reports whether every hook event points at t.
func HooksStatus(home string, t providers.Target) providers.Status {
	st := providers.Status{Name: "codex-hooks", Title: "Codex hooks", File: hooksPath(home), Note: trustNote}
	if err := checkHome(home); err != nil {
		st.Note = err.Error()
		return st
	}
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
			if ours[0]["command"] == hookHandler(t, ev)["command"] {
				current++
			}
		}
	}
	st.Installed = found > 0
	st.Stale = found > 0 && (found < len(hookEvents) || current < len(hookEvents))
	if st.Stale {
		st.Note = "已安装的 hooks 与当前程序路径或端口不一致，请重新安装；" + trustNote
	}
	return st
}

// InstallHooks adds or refreshes go-dock's hooks in hooks.json. Existing
// handlers are replaced in place and new groups appended, because Codex
// keys hook trust by position.
func InstallHooks(home string, t providers.Target) error {
	if err := checkHome(home); err != nil {
		return err
	}
	f, err := providers.ReadJSONFile(hooksPath(home))
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

// UninstallHooks removes go-dock's hooks. A file left with nothing in it
// is deleted (after the usual backup).
func UninstallHooks(home string) error {
	f, err := providers.ReadJSONFile(hooksPath(home))
	if err != nil || !f.Exists {
		return err
	}
	hooks := f.Get("hooks")
	if !hooks.IsObject() {
		return nil
	}
	changed := false
	for ev := range hooks.Map() {
		path := "hooks." + ev
		before := providers.Groups(f.Get(path))
		if len(providers.FindHooks(before, isOurHook)) == 0 {
			continue
		}
		changed = true
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
	if !changed {
		return nil
	}
	if h := f.Get("hooks"); h.IsObject() && len(h.Map()) == 0 {
		if err := f.Delete("hooks"); err != nil {
			return err
		}
	}
	if err := f.Save(); err != nil {
		return err
	}
	if root := f.Get("@this"); root.IsObject() && len(root.Map()) == 0 {
		return os.Remove(f.Path)
	}
	return nil
}
