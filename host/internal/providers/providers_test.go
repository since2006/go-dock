package providers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestToolSummary(t *testing.T) {
	cases := []struct{ tool, input, want string }{
		{"Bash", `{"command":"go test ./..."}`, "Bash: go test ./..."},
		{"Edit", `{"file_path":"/a/b/main.go","old_string":"x"}`, "Edit: main.go"},
		{"Bash", `{"command":["bash","-lc","npm test"]}`, "Bash: npm test"},
		{"mcp__x__y", `{}`, "mcp__x__y"},
		{"", `{}`, ""},
	}
	for _, c := range cases {
		if got := ToolSummary(c.tool, json.RawMessage(c.input)); got != c.want {
			t.Errorf("%s %s = %q, want %q", c.tool, c.input, got, c.want)
		}
	}
}

func TestMergeHookKeepsPositions(t *testing.T) {
	ours := func(h map[string]any) bool { return h["command"] == "go-dock old" || h["command"] == "go-dock new" }
	var groups []HookGroup
	json.Unmarshal([]byte(`[
		{"matcher":"Bash","hooks":[{"command":"user1"},{"command":"go-dock old"}]},
		{"hooks":[{"command":"go-dock old"}]},
		{"hooks":[{"command":"user2"}]}
	]`), &groups)
	out := MergeHook(groups, map[string]any{"command": "go-dock new"}, ours)
	b, _ := json.Marshal(out)
	want := `[{"hooks":[{"command":"user1"},{"command":"go-dock new"}],"matcher":"Bash"},{"hooks":[{"command":"user2"}]}]`
	if string(b) != want {
		t.Fatalf("merged = %s", b)
	}
	if n := len(FindHooks(out, ours)); n != 1 {
		t.Fatalf("found %d", n)
	}
	if out := RemoveHooks(out, ours); len(FindHooks(out, ours)) != 0 || len(out) != 2 {
		t.Fatalf("removed = %+v", out)
	}
}

func TestIsGoDockExe(t *testing.T) {
	for _, p := range []string{"/usr/local/bin/go-dock", `'C:/Tools/go-dock.exe'`, `C:\Tools\go-dock.exe`, "go-dock"} {
		if !IsGoDockExe(p) {
			t.Errorf("%s not recognized", p)
		}
	}
	if IsGoDockExe("/usr/bin/node") {
		t.Error("node recognized")
	}
}

func TestShellQuote(t *testing.T) {
	if got := shQuote("/opt/go-dock/go-dock"); got != "/opt/go-dock/go-dock" {
		t.Errorf("plain = %s", got)
	}
	if got := shQuote("/Users/a b/it's/go-dock"); got != `'/Users/a b/it'\''s/go-dock'` {
		t.Errorf("quoted = %s", got)
	}
	if got := psQuote("C:/a'b/go-dock.exe"); got != `'C:/a''b/go-dock.exe'` {
		t.Errorf("ps = %s", got)
	}
}

func TestSaveKeepsEveryBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(path, []byte(`{"v":0}`), 0o644)
	for i := 1; i <= 3; i++ {
		f, err := ReadJSONFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f.Set("v", i)
		if err := f.Save(); err != nil {
			t.Fatal(err)
		}
	}
	baks, _ := filepath.Glob(path + ".go-dock-*.bak")
	if len(baks) != 3 {
		t.Fatalf("backups = %v", baks)
	}
	found := false
	for _, b := range baks {
		if data, _ := os.ReadFile(b); string(data) == `{"v":0}` {
			found = true
		}
	}
	if !found {
		t.Fatal("original content not among backups")
	}
}
