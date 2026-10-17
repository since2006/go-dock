// Package config loads and saves go-dock's JSON configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const DefaultListen = "127.0.0.1:47321"

// Duration is a time.Duration that marshals as a string such as "30m".
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

type Config struct {
	Web     WebConfig     `json:"web"`
	Device  DeviceConfig  `json:"device"`
	Claude  ClaudeConfig  `json:"claude"`
	Codex   CodexConfig   `json:"codex"`
	Display DisplayConfig `json:"display"`
	Log     LogConfig     `json:"log"`
	// Pricing overrides the built-in USD prices per million tokens, keyed by
	// model name prefix.
	Pricing map[string]Price `json:"pricing,omitempty"`
}

type WebConfig struct {
	Listen      string `json:"listen"`
	OpenBrowser bool   `json:"open_browser"`
}

type DeviceConfig struct {
	Enabled bool `json:"enabled"`
	// Port is "auto" or an explicit serial port path such as /dev/cu.usbmodem1101 or COM3.
	Port string `json:"port"`
}

type ClaudeConfig struct {
	Enabled bool `json:"enabled"`
	// Home overrides the Claude config dir; empty means $CLAUDE_CONFIG_DIR or ~/.claude.
	Home string `json:"home"`
}

type CodexConfig struct {
	Enabled bool `json:"enabled"`
	// Home overrides the Codex home; empty means $CODEX_HOME or ~/.codex.
	Home string `json:"home"`
	// AppServerPoll periodically asks `codex app-server` for rate limits.
	AppServerPoll     bool     `json:"app_server_poll"`
	AppServerInterval Duration `json:"app_server_interval"`
}

type DisplayConfig struct {
	// DoneTTL is how long a finished session stays in the "done" state.
	DoneTTL Duration `json:"done_ttl"`
	// StaleAfter drops sessions with no news for this long.
	StaleAfter  Duration `json:"stale_after"`
	ShowIdle    bool     `json:"show_idle"`
	MaxSessions int      `json:"max_sessions"`
}

type LogConfig struct {
	Level string `json:"level"` // debug, info, warn, error
}

// Price is USD per million tokens.
type Price struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cache_read"`
	CacheWrite5m float64 `json:"cache_write_5m"`
	CacheWrite1h float64 `json:"cache_write_1h"`
}

func Default() Config {
	return Config{
		Web:    WebConfig{Listen: DefaultListen, OpenBrowser: true},
		Device: DeviceConfig{Enabled: true, Port: "auto"},
		Claude: ClaudeConfig{Enabled: true},
		Codex: CodexConfig{
			Enabled:           true,
			AppServerPoll:     true,
			AppServerInterval: Duration(10 * time.Minute),
		},
		Display: DisplayConfig{
			DoneTTL:     Duration(30 * time.Minute),
			StaleAfter:  Duration(12 * time.Hour),
			MaxSessions: 12,
		},
		Log: LogConfig{Level: "info"},
	}
}

// Validate checks values that would otherwise break the daemon.
func (c Config) Validate() error {
	if c.Web.Listen == "" {
		return errors.New("web.listen 不能为空")
	}
	if c.Device.Port == "" {
		return errors.New("device.port 不能为空（自动检测请填 auto）")
	}
	if c.Display.DoneTTL < 0 || c.Display.StaleAfter <= 0 {
		return errors.New("display 时长必须为正数")
	}
	if c.Display.MaxSessions < 1 || c.Display.MaxSessions > 32 {
		return errors.New("display.max_sessions 需在 1-32 之间")
	}
	if c.Codex.AppServerInterval.D() < time.Minute {
		return errors.New("codex.app_server_interval 不能小于 1m")
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("未知日志级别 %q", c.Log.Level)
	}
	return nil
}

// Dir returns the directory holding config.json and logs. GO_DOCK_HOME
// overrides the platform default.
func Dir() (string, error) {
	if d := os.Getenv("GO_DOCK_HOME"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "go-dock"), nil
}

// Store guards the current config and persists changes.
type Store struct {
	path string

	setMu     sync.Mutex // serializes Set so saves land in order
	mu        sync.RWMutex
	cfg       Config
	listeners []func(old, cur Config)
}

// Load reads path, filling missing fields with defaults. A missing file is
// not an error; it is created with defaults.
func Load(path string) (*Store, error) {
	s := &Store{path: path, cfg: Default()}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, s.save(s.cfg)
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(b, &s.cfg); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", path, err)
	}
	if err := s.cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Set validates, saves and publishes a new config.
func (s *Store) Set(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	s.setMu.Lock()
	defer s.setMu.Unlock()
	if err := s.save(c); err != nil {
		return err
	}
	s.mu.Lock()
	old := s.cfg
	s.cfg = c
	ls := append([]func(old, cur Config){}, s.listeners...)
	s.mu.Unlock()
	for _, f := range ls {
		f(old, c)
	}
	return nil
}

// OnChange registers f to run after every successful Set.
func (s *Store) OnChange(f func(old, cur Config)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners = append(s.listeners, f)
}

func (s *Store) save(c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(s.path, append(b, '\n'), 0o600)
}

// WriteFileAtomic writes data to a temp file next to path and renames it
// into place, keeping the existing file mode when there is one.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	if fi, err := os.Stat(path); err == nil {
		perm = fi.Mode().Perm()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
