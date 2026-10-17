package providers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/pretty"
	"github.com/tidwall/sjson"

	"github.com/since2006/go-dock/host/internal/config"
)

// Status describes one integration as shown on the web UI.
type Status struct {
	Name      string `json:"name"`
	Title     string `json:"title"`
	File      string `json:"file"`
	Installed bool   `json:"installed"`
	// Stale means entries exist but point at another executable or address.
	Stale bool   `json:"stale"`
	Note  string `json:"note,omitempty"`
	Error string `json:"error,omitempty"`
}

// Target is what installed hooks call back into.
type Target struct {
	Exe  string // absolute path of the go-dock executable
	Addr string // daemon listen address
}

// CurrentTarget resolves the running executable. It refuses binaries that
// `go run` builds in a temp dir, since hooks would break once it exits.
func CurrentTarget(addr string) (Target, error) {
	exe, err := os.Executable()
	if err != nil {
		return Target{}, err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if strings.Contains(exe, "go-build") || strings.HasPrefix(exe, os.TempDir()) {
		return Target{}, errors.New("当前是 go run 生成的临时程序，请先 go build 出固定位置的可执行文件再安装集成")
	}
	return Target{Exe: exe, Addr: addr}, nil
}

// IsGoDockExe reports whether path names a go-dock binary.
func IsGoDockExe(path string) bool {
	path = strings.Trim(path, `"'& `)
	base := strings.ToLower(filepath.Base(filepath.FromSlash(strings.ReplaceAll(path, `\`, "/"))))
	return strings.HasPrefix(base, "go-dock")
}

// ShellCommand renders exe plus args as a command line for the platform's
// default hook shell: POSIX sh on Unix, PowerShell on Windows.
func ShellCommand(exe string, args ...string) string {
	if runtime.GOOS == "windows" {
		return "& " + psQuote(filepath.ToSlash(exe)) + " " + strings.Join(args, " ")
	}
	return shQuote(exe) + " " + strings.Join(args, " ")
}

// PortableCommand renders a command line that works in both Git Bash and
// PowerShell on Windows, as long as the path has no spaces.
func PortableCommand(exe string, args ...string) string {
	if runtime.GOOS == "windows" {
		p := filepath.ToSlash(exe)
		if strings.ContainsAny(p, " '\"") {
			p = `"` + p + `"`
		}
		return p + " " + strings.Join(args, " ")
	}
	return shQuote(exe) + " " + strings.Join(args, " ")
}

func shQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./:+@%") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// JSONFile edits a JSON settings file while keeping unrelated content and
// key order.
type JSONFile struct {
	Path   string
	Data   []byte
	Exists bool
}

func ReadJSONFile(path string) (*JSONFile, error) {
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &JSONFile{Path: path, Data: []byte("{}")}, nil
	case err != nil:
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		b = []byte("{}")
	}
	if !gjson.ValidBytes(b) {
		return nil, fmt.Errorf("%s 不是合法的 JSON，未做修改", path)
	}
	return &JSONFile{Path: path, Data: b, Exists: true}, nil
}

func (f *JSONFile) Get(path string) gjson.Result { return gjson.GetBytes(f.Data, path) }

// Set replaces the value at path with v marshaled to JSON.
func (f *JSONFile) Set(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f.Data, err = sjson.SetRawBytes(f.Data, path, raw)
	return err
}

func (f *JSONFile) Delete(path string) error {
	var err error
	f.Data, err = sjson.DeleteBytes(f.Data, path)
	return err
}

// Save backs up the original file and writes the new content atomically.
func (f *JSONFile) Save() error {
	out := pretty.PrettyOptions(f.Data, &pretty.Options{Width: 80, Indent: "  "})
	if f.Exists {
		orig, err := os.ReadFile(f.Path)
		if err != nil {
			return err
		}
		if err := backup(f.Path, orig); err != nil {
			return fmt.Errorf("备份 %s: %w", f.Path, err)
		}
	}
	return config.WriteFileAtomic(f.Path, out, 0o644)
}

// backup writes data next to path under a new name; it never overwrites an
// earlier backup, even when several edits land within the same second.
func backup(path string, data []byte) error {
	base := path + ".go-dock-" + time.Now().Format("20060102-150405")
	for i := 0; i < 100; i++ {
		name := base + ".bak"
		if i > 0 {
			name = fmt.Sprintf("%s-%d.bak", base, i)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
	return errors.New("备份文件过多")
}

// HookGroup is one matcher group of a hooks file; both Claude Code and
// Codex use {"matcher": ..., "hooks": [handler...]}.
type HookGroup = map[string]any

// MergeHook places handler into groups: the first existing handler that
// isOurs accepts is replaced in place (so positions stay stable), others
// of ours are dropped, and a new group is appended when there was none.
func MergeHook(groups []HookGroup, handler map[string]any, isOurs func(map[string]any) bool) []HookGroup {
	placed := false
	out := groups[:0:0]
	for _, g := range groups {
		hs, _ := g["hooks"].([]any)
		kept := make([]any, 0, len(hs))
		for _, h := range hs {
			hm, ok := h.(map[string]any)
			if ok && isOurs(hm) {
				if placed {
					continue
				}
				placed = true
				kept = append(kept, handler)
				continue
			}
			kept = append(kept, h)
		}
		if len(kept) == 0 {
			continue
		}
		g["hooks"] = kept
		out = append(out, g)
	}
	if !placed {
		out = append(out, HookGroup{"hooks": []any{handler}})
	}
	return out
}

// RemoveHooks drops every handler isOurs accepts and empty groups.
func RemoveHooks(groups []HookGroup, isOurs func(map[string]any) bool) []HookGroup {
	out := groups[:0:0]
	for _, g := range groups {
		hs, _ := g["hooks"].([]any)
		kept := make([]any, 0, len(hs))
		for _, h := range hs {
			if hm, ok := h.(map[string]any); ok && isOurs(hm) {
				continue
			}
			kept = append(kept, h)
		}
		if len(kept) == 0 {
			continue
		}
		g["hooks"] = kept
		out = append(out, g)
	}
	return out
}

// FindHooks returns our handlers in groups.
func FindHooks(groups []HookGroup, isOurs func(map[string]any) bool) []map[string]any {
	var out []map[string]any
	for _, g := range groups {
		hs, _ := g["hooks"].([]any)
		for _, h := range hs {
			if hm, ok := h.(map[string]any); ok && isOurs(hm) {
				out = append(out, hm)
			}
		}
	}
	return out
}

// Groups decodes a hooks array; anything malformed yields nil.
func Groups(r gjson.Result) []HookGroup {
	if !r.IsArray() {
		return nil
	}
	var gs []HookGroup
	if json.Unmarshal([]byte(r.Raw), &gs) != nil {
		return nil
	}
	return gs
}
