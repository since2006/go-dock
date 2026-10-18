// Package config loads and saves go-dock's JSON configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	MQTT    MQTTConfig    `json:"mqtt"`
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
	// Port is "auto" or an explicit serial port path such as /dev/cu.usbmodem1101 or COM3.
	Port string `json:"port"`
	// Beep sounds the buzzer wired to the screen when a task starts waiting
	// for the user (approval or an answer); BeepSound picks the melody, an
	// id from device.Sounds (unknown ids play the default).
	Beep      bool   `json:"beep"`
	BeepSound string `json:"beep_sound"`
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

// MQTTConfig lists the brokers plugins can use; each keeps its own
// connection and plugins refer to one by id.
type MQTTConfig struct {
	Brokers List[Broker] `json:"brokers"`
}

// MaxBrokers bounds the connections go-dock keeps open.
const MaxBrokers = 8

type Broker struct {
	// ID is how plugins refer to the broker; the web UI fixes it once
	// saved.
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// URL is mqtt://host:1883, mqtts://host:8883, ws:// or wss://; a bare
	// host[:port] means mqtt://.
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
	// ClientID defaults to go-dock-<host>-<random>.
	ClientID string `json:"client_id"`
}

// Label names the broker in messages: its name, else its id.
func (b Broker) Label() string {
	if n := strings.TrimSpace(b.Name); n != "" {
		return n
	}
	return b.ID
}

// BrokerFor resolves a plugin's broker reference: an empty id means the
// first broker. It returns "" when there is none.
func (m MQTTConfig) BrokerFor(id string) string {
	if id == "" && len(m.Brokers) > 0 {
		return m.Brokers[0].ID
	}
	return id
}

// PluginsConfig holds one section per function-page plugin.
type PluginsConfig struct {
	Lights LightsConfig `json:"lights"`
	Aircon AirconConfig `json:"aircon"`
}

// AirconConfig is the air conditioner page: a Gree unit the screen
// switches by infrared.
type AirconConfig struct {
	Enabled bool `json:"enabled"`
	// Name is the page title, at most 8 characters.
	Name string `json:"name"`
	// Model is the Gree remote to imitate: yaw1f (most) or ybofb.
	Model string `json:"model"`
}

func (a AirconConfig) validate() error {
	if utf8.RuneCountInString(strings.TrimSpace(a.Name)) > 8 {
		return errors.New("空调名称最多 8 个字")
	}
	switch a.Model {
	case "", "yaw1f", "ybofb":
	default:
		return fmt.Errorf("不认识的格力遥控器型号 %q", a.Model)
	}
	return nil
}

type LightsConfig struct {
	Enabled bool        `json:"enabled"`
	Lights  List[Light] `json:"lights"`
}

// MaxLights is what the lights page fits on the screen.
const MaxLights = 6

// Light is one lamp switched over MQTT. Empty payloads mean "ON"/"OFF";
// empty state values mean the payloads.
type Light struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Broker is the id of the MQTT broker the lamp is on; empty means the
	// first one.
	Broker       string `json:"broker"`
	CommandTopic string `json:"command_topic"`
	PayloadOn    string `json:"payload_on"`
	PayloadOff   string `json:"payload_off"`
	// StateTopic reports the lamp's state; without one the last command
	// sent counts as the state.
	StateTopic string `json:"state_topic"`
	// StatePath picks the state out of a JSON payload (gjson syntax, such
	// as "state" or "POWER1"); empty compares the whole payload.
	StatePath string `json:"state_path"`
	StateOn   string `json:"state_on"`
	StateOff  string `json:"state_off"`
	// QueryTopic, if set, gets QueryPayload on every connect so the lamp
	// reports its state.
	QueryTopic   string `json:"query_topic"`
	QueryPayload string `json:"query_payload"`
}

// List is a config array that replaces itself on decode instead of
// decoding into the old elements, so a shorter list, or an entry without
// some keys, keeps nothing from before. It is written as [] when empty.
type List[T any] []T

func (l *List[T]) UnmarshalJSON(b []byte) error {
	var fresh []T
	if err := json.Unmarshal(b, &fresh); err != nil {
		return err
	}
	*l = fresh
	return nil
}

func (l List[T]) MarshalJSON() ([]byte, error) {
	if l == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]T(l))
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
		Device: DeviceConfig{Enabled: true, Port: "auto", Beep: true, BeepSound: "chime"},
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
		Log:    LogConfig{Level: "info"},
		Update: UpdateConfig{AutoCheck: true},
		MQTT:   MQTTConfig{Brokers: List[Broker]{}},
		Plugins: PluginsConfig{
			Lights: LightsConfig{Lights: List[Light]{}},
			Aircon: AirconConfig{Name: "空调", Model: "yaw1f"},
		},
	}
}

// Clone returns a copy that shares no maps or slices with c, so decoding
// onto it (as POST /api/config does) cannot change c.
func (c Config) Clone() Config {
	if c.Pricing != nil {
		c.Pricing = maps.Clone(c.Pricing)
	}
	c.MQTT.Brokers = slices.Clone(c.MQTT.Brokers)
	c.Plugins.Lights.Lights = slices.Clone(c.Plugins.Lights.Lights)
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
	if err := c.MQTT.validate(); err != nil {
		return err
	}
	if err := c.Plugins.Lights.validate(c.MQTT); err != nil {
		return err
	}
	return c.Plugins.Aircon.validate()
}

// configID is what broker and light ids look like; the screen protocol
// takes light ids as they are.
var configID = regexp.MustCompile(`^[a-z0-9_-]{1,16}$`)

func (m MQTTConfig) validate() error {
	if len(m.Brokers) > MaxBrokers {
		return fmt.Errorf("最多 %d 个 MQTT broker", MaxBrokers)
	}
	seen := map[string]bool{}
	for i, b := range m.Brokers {
		where := fmt.Sprintf("第 %d 个 broker", i+1)
		switch {
		case !configID.MatchString(b.ID):
			return fmt.Errorf("%s的 id %q 只能用小写字母、数字、- 和 _，最长 16 位", where, b.ID)
		case seen[b.ID]:
			return fmt.Errorf("broker 的 id %q 重复", b.ID)
		case utf8.RuneCountInString(b.Name) > 24:
			return fmt.Errorf("%s的名称不能超过 24 个字", where)
		case len(b.ClientID) > 64:
			return fmt.Errorf("%s的 client id 不能超过 64 字节", where)
		}
		seen[b.ID] = true
		if b.Enabled || strings.TrimSpace(b.URL) != "" {
			if _, err := BrokerURL(b.URL); err != nil {
				return fmt.Errorf("%s「%s」：%w", where, b.Label(), err)
			}
		}
	}
	return nil
}

func (l LightsConfig) validate(m MQTTConfig) error {
	if len(l.Lights) > MaxLights {
		return fmt.Errorf("最多 %d 盏灯", MaxLights)
	}
	seen := map[string]bool{}
	for i, x := range l.Lights {
		where := fmt.Sprintf("第 %d 盏灯", i+1)
		switch {
		case !configID.MatchString(x.ID):
			return fmt.Errorf("%s的 id %q 只能用小写字母、数字、- 和 _，最长 16 位", where, x.ID)
		case seen[x.ID]:
			return fmt.Errorf("灯的 id %q 重复", x.ID)
		case strings.TrimSpace(x.Name) == "":
			return fmt.Errorf("%s缺少名称", where)
		case utf8.RuneCountInString(x.Name) > 24:
			return fmt.Errorf("%s的名称不能超过 24 个字", where)
		case x.CommandTopic == "":
			return fmt.Errorf("%s缺少命令主题", where)
		case x.Broker != "" && !slices.ContainsFunc(m.Brokers, func(b Broker) bool { return b.ID == x.Broker }):
			return fmt.Errorf("「%s」用的 MQTT broker %q 不存在（删除 broker 前先把用它的灯改到别的 broker）", x.Name, x.Broker)
		}
		seen[x.ID] = true
		for _, t := range [][2]string{{"命令主题", x.CommandTopic}, {"状态主题", x.StateTopic}, {"查询主题", x.QueryTopic}} {
			if strings.ContainsAny(t[1], "+#\x00") {
				return fmt.Errorf("%s的%s不能包含通配符 + 或 #", where, t[0])
			}
			if len(t[1]) > 256 {
				return fmt.Errorf("%s的%s太长", where, t[0])
			}
		}
		for _, v := range []string{x.PayloadOn, x.PayloadOff, x.StatePath, x.StateOn, x.StateOff, x.QueryPayload} {
			if len(v) > 256 {
				return fmt.Errorf("%s的负载或状态值太长（最多 256 字节）", where)
			}
		}
	}
	return nil
}

// BrokerURL normalizes an MQTT broker address: a bare host[:port] means
// mqtt://, and plain and TLS brokers get the default port when it is left
// out.
func BrokerURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("broker 地址不能为空")
	}
	if !strings.Contains(s, "://") {
		s = "mqtt://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("broker 地址格式不对: %w", err)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	port := ""
	switch u.Scheme {
	case "mqtt", "tcp":
		port = "1883"
	case "mqtts", "ssl", "tls":
		port = "8883"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("broker 地址不支持 %s://，请用 mqtt://、mqtts://、ws:// 或 wss://", u.Scheme)
	}
	switch {
	case u.User != nil:
		return "", errors.New("broker 地址里不要写用户名和密码，请填在对应的输入框里")
	case u.Hostname() == "":
		return "", errors.New("broker 地址缺少主机名")
	case port != "" && (strings.Trim(u.Path, "/") != "" || u.RawQuery != ""):
		return "", errors.New("broker 地址只需要主机和端口，不能带路径")
	}
	if port != "" {
		u.Path, u.RawPath = "", ""
		if u.Port() == "" {
			u.Host = net.JoinHostPort(u.Hostname(), port)
		}
	}
	return u.String(), nil
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
