package codex

import (
	"cmp"
	"encoding/json"
	"path"
	"slices"
	"strings"

	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/providers"
)

// toolName flattens a namespaced tool the way Codex hooks name it: MCP
// tools become mcp__<server>__<tool>.
func toolName(namespace, name string) string {
	if strings.HasPrefix(namespace, "mcp__") {
		return strings.TrimSuffix(namespace, "__") + "__" + name
	}
	return name
}

// toolDetail renders a Codex tool call for the detail line, like
// providers.ToolSummary does for Claude Code: shell commands as
// "Bash: go test ./...", the name Codex hooks give them, and patches by the
// files they touch. "" means the call changes nothing worth showing
// (write_stdin feeds a command that is already running).
func toolDetail(name string, input json.RawMessage) string {
	switch name {
	case "write_stdin":
		return ""
	case "exec_command", "shell", "shell_command", "local_shell":
		name = "Bash"
	case "apply_patch":
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(input, &in)
		if files := patchFiles(in.Command); files != "" {
			return model.Truncate(name+": "+files, providers.DetailLen)
		}
	case "spawn_agent":
		var in struct {
			TaskName string `json:"task_name"`
			Message  string `json:"message"`
		}
		_ = json.Unmarshal(input, &in)
		if task := cmp.Or(in.TaskName, in.Message); task != "" {
			return model.Truncate(name+": "+task, providers.DetailLen)
		}
	}
	return providers.ToolSummary(name, input)
}

// patchFiles lists the files an apply_patch patch adds, updates or
// deletes, by base name.
func patchFiles(patch string) string {
	var files []string
	for _, l := range strings.Split(patch, "\n") {
		for _, p := range []string{"*** Update File: ", "*** Add File: ", "*** Delete File: "} {
			if f, ok := strings.CutPrefix(strings.TrimSpace(l), p); ok && f != "" {
				f = path.Base(strings.ReplaceAll(f, `\`, "/"))
				if !slices.Contains(files, f) {
					files = append(files, f)
				}
			}
		}
	}
	return strings.Join(files, ", ")
}
