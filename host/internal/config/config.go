// Package config loads and saves go-dock's JSON configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const DefaultListen = "127.0.0.1:47321"

// Duration is a time.Duration that marshals as a string such as "30m".
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

// String renders whole minutes and hours without zero units: "30m", "12h".
func (d Duration) String() string {
	s := time.Duration(d).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
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
	Update  UpdateConfig  `json:"update"`
	Plugins PluginsConfig `json:"plugins"`
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
	// Addr ("ip" or "ip:port", port 47322 by default) and Code, the
	// pairing code the screen shows, reach the screen over WiFi.
	Addr string `json:"addr"`
	Code string `json:"code"`
	// Port is the USB cable that sets up the screen's WiFi and updates its
	// firmware: "auto" or a serial port path such as /dev/cu.usbmodem1101
	// or COM3.
	Port string `json:"port"`
	// Name is this computer's name on the screen; empty uses the hostname.
	Name string `json:"name"`
	// Beep sounds the buzzer wired to the screen when a task starts waiting
	// for the user (approval or an answer); BeepSound picks the melody, an
	// id from device.Sounds (unknown ids play the default) or "custom" for
	// the notes in BeepCustom (device.ParseNotes: "E7 100, - 40, C8 300").
	Beep       bool   `json:"beep"`
	BeepSound  string `json:"beep_sound"`
	BeepCustom string `json:"beep_custom"`
	// BeepDone sounds it when a task finishes, with a melody of its own
	// picked the same way.
	BeepDone       bool   `json:"beep_done"`
	BeepDoneSound  string `json:"beep_done_sound"`
	BeepDoneCustom string `json:"beep_done_custom"`
	// Mute silences both for a while, keeping which are on; the 屏幕 page
	// switches it.
	Mute bool `json:"mute"`
	// Volume is how loud both play, in percent (5-100).
	Volume int `json:"volume"`
	// Brightness is the screen's brightness in percent. It dims to
	// DimBrightness (0 turns it off) after DimAfter without an active task;
	// a DimAfter of 0 never dims.
	Brightness    int      `json:"brightness"`
	DimBrightness int      `json:"dim_brightness"`
	DimAfter      Duration `json:"dim_after"`
}

// deviceNameLen bounds DeviceConfig.Name, in runes: device.NameLen.
const deviceNameLen = 20

var pairingCodeRE = regexp.MustCompile(`^[0-9]{6}$`)

func (d DeviceConfig) validate() error {
	// Both empty: no screen set up yet.
	addr := strings.TrimSpace(d.Addr)
	switch {
	case addr == "" && d.Code != "":
		return errors.New("请填写屏幕地址")
	case addr != "" && !pairingCodeRE.MatchString(d.Code):
		return errors.New("配对码是屏幕上显示的 6 位数字")
	}
	switch {
	case utf8.RuneCountInString(strings.TrimSpace(d.Name)) > deviceNameLen:
		return fmt.Errorf("本机名称最多 %d 个字", deviceNameLen)
	case d.Brightness < 10 || d.Brightness > 100:
		return errors.New("屏幕亮度需在 10%-100% 之间")
	case d.DimBrightness < 0 || d.DimBrightness > 100:
		return errors.New("变暗后的亮度需在 0%-100% 之间")
	case d.DimAfter != 0 && (d.DimAfter.D() < time.Minute || d.DimAfter.D() > 24*time.Hour):
		return errors.New("变暗时间需在 1m-24h 之间，0 为不变暗")
	case d.Volume < 5 || d.Volume > 100:
		return errors.New("提示音音量需在 5%-100% 之间")
	}
	return nil
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

type UpdateConfig struct {
	// AutoCheck looks for a new release once a day.
	AutoCheck bool `json:"auto_check"`
	// Prerelease also offers test releases (tags like v0.3.0-beta.1).
	Prerelease bool `json:"prerelease"`
}

type LogConfig struct {
	Level string `json:"level"` // debug, info, warn, error
}

// PluginsConfig holds one section per plugin of the Agent page's line.
type PluginsConfig struct {
	Perf PerfConfig `json:"perf"`
}

// PerfConfig is 性能: the computer's CPU, GPU, memory, fans, network and
// disk in the line.
type PerfConfig struct {
	Enabled bool `json:"enabled"`
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
		Web: WebConfig{Listen: DefaultListen, OpenBrowser: true},
		Device: DeviceConfig{
			Enabled: true, Port: "auto", Beep: true, BeepSound: "chime", BeepDoneSound: "rise", Volume: 100,
			Brightness: 80, DimBrightness: 20, DimAfter: Duration(5 * time.Minute),
		},
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
		Log:     LogConfig{Level: "info"},
		Update:  UpdateConfig{AutoCheck: true},
		Plugins: PluginsConfig{},
	}
}

// Clone returns a copy that shares no maps or slices with c, so decoding
// onto it (as POST /api/config does) cannot change c.
func (c Config) Clone() Config {
	if c.Pricing != nil {
		c.Pricing = maps.Clone(c.Pricing)
	}
	return c
}

// Validate checks values that would otherwise break the daemon.
func (c Config) Validate() error {
	if c.Web.Listen == "" {
		return errors.New("web.listen 不能为空")
	}
	if c.Device.Port == "" {
		return errors.New("device.port 不能为空（自动检测请填 auto）")
	}
	if err := c.Device.validate(); err != nil {
		return err
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
	return s.cfg.Clone()
}

// Set validates, saves and publishes a new config.
func (s *Store) Set(c Config) error {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.set(c)
}

// Update changes the current config with f and sets the result, with no
// other change in between.
func (s *Store) Update(f func(*Config)) error {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	c := s.Get()
	f(&c)
	return s.set(c)
}

func (s *Store) set(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
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
