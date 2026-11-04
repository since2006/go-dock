package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/since2006/go-dock/host/internal/config"
	"github.com/since2006/go-dock/host/internal/logx"
	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/providers"
)

type fakeBackend struct {
	cfg       config.Config
	ingests   []string
	autostart bool
	flashing  bool
	beeps     [][][2]int
	volumes   []int
	wifi      []WiFi
}

func (f *fakeBackend) Snapshot() model.Snapshot             { return model.Snapshot{} }
func (f *fakeBackend) Subscribe() (<-chan struct{}, func()) { return make(chan struct{}), func() {} }
func (f *fakeBackend) Config() config.Config                { return f.cfg.Clone() }
func (f *fakeBackend) SetConfig(c config.Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	f.cfg = c
	return nil
}
func (f *fakeBackend) Integrations() []providers.Status      { return nil }
func (f *fakeBackend) Integration(name, action string) error { return nil }
func (f *fakeBackend) Ingest(p, k string, body []byte) error {
	f.ingests = append(f.ingests, p+"/"+k+":"+string(body))
	return nil
}
func (f *fakeBackend) DeviceStatus() any         { return map[string]string{"state": "disabled"} }
func (f *fakeBackend) DeviceFrame() any          { return nil }
func (f *fakeBackend) DevicePorts() (any, error) { return []string{}, nil }
func (f *fakeBackend) ReconnectDevice()          {}
func (f *fakeBackend) DeviceWiFiScan(ctx context.Context) (any, error) {
	if f.cfg.Device.Port == "none" {
		return nil, errors.New("屏幕未连接")
	}
	return []map[string]any{{"ssid": "home", "rssi": -48}, {"ssid": "cafe", "rssi": -80, "open": true}}, nil
}
func (f *fakeBackend) DeviceWiFi(w WiFi) (int, error) {
	if w.SSID == "busy" {
		return 0, errors.New("屏幕未连接")
	}
	f.wifi = append(f.wifi, w)
	return len(f.wifi), nil
}
func (f *fakeBackend) ApplyFirmware() error {
	if f.flashing {
		return errors.New("固件正在升级")
	}
	f.flashing = true
	return nil
}
func (f *fakeBackend) UpdateStatus() any  { return map[string]string{"state": "idle"} }
func (f *fakeBackend) CheckUpdate() error { return nil }
func (f *fakeBackend) ApplyUpdate() error { return errors.New("没有可用的新版本") }
func (f *fakeBackend) System() any        { return map[string]any{"autostart": f.autostart} }
func (f *fakeBackend) SetAutostart(on bool) error {
	f.autostart = on
	return nil
}
func (f *fakeBackend) DeviceLine() any { return map[string]any{"t": "line", "items": []any{}} }
func (f *fakeBackend) DeviceSounds() any {
	return []map[string]string{{"id": "chime", "name": "叮咚"}}
}
func (f *fakeBackend) SoundNotes(sound, custom string) ([][2]int, error) {
	switch sound {
	case "custom":
		if custom == "" {
			return nil, errors.New("还没有填音符")
		}
		return [][2]int{{2637, 100}}, nil
	case "rise", "":
		return [][2]int{{2093, 100}}, nil
	}
	return nil, errors.New("no such sound")
}
func (f *fakeBackend) DeviceBeep(notes [][2]int, volume int) error {
	if f.cfg.Device.Port == "none" {
		return errors.New("设备未连接")
	}
	f.beeps = append(f.beeps, notes)
	f.volumes = append(f.volumes, volume)
	return nil
}

func newTestServer(listen string) (*fakeBackend, http.Handler) {
	b := &fakeBackend{cfg: config.Default()}
	s := New(listen, b, logx.NewRing(10), slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	return b, s.Handler()
}

func do(h http.Handler, method, host, path, ctype, body string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = host
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestGuard(t *testing.T) {
	_, h := newTestServer("127.0.0.1:47321")
	if w := do(h, "GET", "evil.example:47321", "/api/state", "", ""); w.Code != http.StatusForbidden {
		t.Fatalf("foreign host: %d", w.Code)
	}
	for _, host := range []string{"127.0.0.1:47321", "localhost:47321", "[::1]:47321"} {
		if w := do(h, "GET", host, "/api/state", "", ""); w.Code != http.StatusOK {
			t.Fatalf("host %s: %d", host, w.Code)
		}
	}
	if w := do(h, "POST", "127.0.0.1:47321", "/api/device/reconnect", "text/plain", "{}"); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain post: %d", w.Code)
	}
	if w := do(h, "POST", "127.0.0.1:47321", "/api/device/reconnect", "application/json", "{}", "Sec-Fetch-Site", "cross-site"); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site post: %d", w.Code)
	}

	// A LAN listen address skips the host check.
	_, lan := newTestServer("0.0.0.0:47321")
	if w := do(lan, "GET", "192.168.1.5:47321", "/api/state", "", ""); w.Code != http.StatusOK {
		t.Fatalf("lan host: %d", w.Code)
	}
}

func TestIngestEmptyBody(t *testing.T) {
	b, h := newTestServer("127.0.0.1:47321")
	w := do(h, "POST", "127.0.0.1:47321", "/api/ingest/claude/hook", "application/json", `{"session_id":"s"}`)
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("ingest: %d %q", w.Code, w.Body.String())
	}
	if len(b.ingests) != 1 || b.ingests[0] != `claude/hook:{"session_id":"s"}` {
		t.Fatalf("ingests = %v", b.ingests)
	}
}

func TestConfigSave(t *testing.T) {
	b, h := newTestServer("127.0.0.1:47321")
	w := do(h, "POST", "127.0.0.1:47321", "/api/config", "application/json", `{"display":{"max_sessions":8,"done_ttl":"10m"}}`)
	if w.Code != http.StatusOK || b.cfg.Display.MaxSessions != 8 || b.cfg.Display.DoneTTL.D().Minutes() != 10 {
		t.Fatalf("save: %d %s %+v", w.Code, w.Body.String(), b.cfg.Display)
	}
	if b.cfg.Web.Listen != config.DefaultListen {
		t.Fatal("partial save cleared other fields")
	}
	if w := do(h, "POST", "127.0.0.1:47321", "/api/config", "application/json", `{"display":{"max_sessions":0}}`); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid save: %d", w.Code)
	}
	if w := do(h, "POST", "127.0.0.1:47321", "/api/config", "application/json", `{"nope":1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", w.Code)
	}
	// Saving is POST only: some networks block PUT.
	if w := do(h, "PUT", "127.0.0.1:47321", "/api/config", "application/json", `{}`); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT: %d", w.Code)
	}
}

func TestConfigSavePerf(t *testing.T) {
	b, h := newTestServer("127.0.0.1:47321")
	host := "127.0.0.1:47321"
	if w := do(h, "POST", host, "/api/config", "application/json", `{"plugins":{"perf":{"enabled":true}}}`); w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if !b.cfg.Plugins.Perf.Enabled {
		t.Fatalf("perf %+v", b.cfg.Plugins)
	}
}

// The state carries the line the screen gets, for the preview.
func TestStateLine(t *testing.T) {
	_, h := newTestServer("127.0.0.1:47321")
	host := "127.0.0.1:47321"
	w := do(h, "GET", host, "/api/state", "", "")
	if !strings.Contains(w.Body.String(), `"line":{"items":[],"t":"line"}`) || strings.Contains(w.Body.String(), `"pages"`) {
		t.Fatalf("state: %s", w.Body.String())
	}
}

func TestBeep(t *testing.T) {
	b, h := newTestServer("127.0.0.1:47321")
	host := "127.0.0.1:47321"
	if w := do(h, "GET", host, "/api/device/sounds", "", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"chime"`) {
		t.Fatalf("sounds: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "POST", host, "/api/device/beep", "application/json", `{"sound":"rise"}`); w.Code != http.StatusOK || len(b.beeps) != 1 || b.beeps[0][0] != [2]int{2093, 100} {
		t.Fatalf("beep: %d %s %v", w.Code, w.Body.String(), b.beeps)
	}
	if w := do(h, "POST", host, "/api/device/beep", "application/json", `{"sound":"custom","custom":"E7 100","volume":40}`); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"notes":[[2637,100]]`) {
		t.Fatalf("custom: %d %s", w.Code, w.Body.String())
	}
	// The volume chosen in the form, saved or not; none means the one set.
	if !slices.Equal(b.volumes, []int{0, 40}) {
		t.Fatalf("volumes %v", b.volumes)
	}
	if w := do(h, "POST", host, "/api/device/beep", "application/json", `{"sound":"rise","volume":101}`); w.Code != http.StatusBadRequest || len(b.beeps) != 2 {
		t.Fatalf("loud: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "POST", host, "/api/device/beep", "application/json", `{"sound":"custom"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "还没有填音符") {
		t.Fatalf("empty custom: %d %s", w.Code, w.Body.String())
	}
	// No device: the notes come back for the browser to play.
	b.cfg.Device.Port = "none"
	if w := do(h, "POST", host, "/api/device/beep", "application/json", `{}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "设备未连接") || !strings.Contains(w.Body.String(), `"notes":[[2093,100]]`) {
		t.Fatalf("no device: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "POST", host, "/api/device/beep", "application/json", `nope`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad body: %d", w.Code)
	}

	// Only the notes, for the browser: nothing goes to the device.
	n := len(b.beeps)
	if w := do(h, "POST", host, "/api/device/notes", "application/json", `{"sound":"custom","custom":"E7 100"}`); w.Code != http.StatusOK || w.Body.String() != `{"notes":[[2637,100]]}`+"\n" || len(b.beeps) != n {
		t.Fatalf("notes: %d %q %d beeps", w.Code, w.Body.String(), len(b.beeps))
	}
	if w := do(h, "POST", host, "/api/device/notes", "application/json", `{"sound":"custom"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "还没有填音符") {
		t.Fatalf("bad notes: %d %s", w.Code, w.Body.String())
	}
}

func TestStaticEmbedded(t *testing.T) {
	_, h := newTestServer("127.0.0.1:47321")
	for _, p := range []string{"/", "/app.js", "/app.css"} {
		if w := do(h, "GET", "127.0.0.1:47321", p, "", ""); w.Code != http.StatusOK || w.Body.Len() == 0 {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
}

func TestSystemEndpoints(t *testing.T) {
	b, h := newTestServer("127.0.0.1:47321")
	if w := do(h, "POST", "127.0.0.1:47321", "/api/autostart", "application/json", `{"enabled":true}`); w.Code != http.StatusOK || !b.autostart {
		t.Fatalf("autostart: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "POST", "127.0.0.1:47321", "/api/update/apply", "application/json", `{}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "没有可用") {
		t.Fatalf("apply: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "127.0.0.1:47321", "/api/state", "", ""); !strings.Contains(w.Body.String(), `"update":`) {
		t.Fatalf("state lacks update: %s", w.Body.String())
	}
	if w := do(h, "POST", "127.0.0.1:47321", "/api/device/firmware", "application/json", `{}`); w.Code != http.StatusAccepted || !b.flashing {
		t.Fatalf("firmware: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "POST", "127.0.0.1:47321", "/api/device/firmware", "application/json", `{}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "正在升级") {
		t.Fatalf("firmware again: %d %s", w.Code, w.Body.String())
	}
}

func TestShutdownEndsEventStreams(t *testing.T) {
	b := &fakeBackend{cfg: config.Default()}
	srv := New("127.0.0.1:0", b, logx.NewRing(10), slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("shutdown took %v with an open event stream", d)
	}
}

func TestWiFi(t *testing.T) {
	b, h := newTestServer("127.0.0.1:47321")
	host := "127.0.0.1:47321"
	body := `{"ssid":"home","pass":"secret123","ip":"192.168.1.50","mask":"255.255.255.0","gw":"192.168.1.1"}`
	// The answer numbers the try, for the page to wait on its outcome.
	if w := do(h, "POST", host, "/api/device/wifi", "application/json", body); w.Code != http.StatusOK || len(b.wifi) != 1 ||
		!strings.Contains(w.Body.String(), `"try":1`) || !strings.Contains(w.Body.String(), `"device":{"state":"disabled"}`) {
		t.Fatalf("wifi: %d %s", w.Code, w.Body.String())
	}
	if got := b.wifi[0]; got != (WiFi{SSID: "home", Pass: "secret123", IP: "192.168.1.50", Mask: "255.255.255.0", GW: "192.168.1.1"}) {
		t.Fatalf("passed %+v", got)
	}
	if w := do(h, "POST", host, "/api/device/wifi", "application/json", `{"ssid":"busy"}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "屏幕未连接") {
		t.Fatalf("refused: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "POST", host, "/api/device/wifi", "application/json", `nope`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad body: %d", w.Code)
	}
	// Same guard as every other write: another site cannot set the WiFi.
	if w := do(h, "POST", host, "/api/device/wifi", "application/json", body, "Sec-Fetch-Site", "cross-site"); w.Code != http.StatusForbidden || len(b.wifi) != 1 {
		t.Fatalf("cross-site: %d", w.Code)
	}
}

func TestWiFiScan(t *testing.T) {
	b, h := newTestServer("127.0.0.1:47321")
	host := "127.0.0.1:47321"
	w := do(h, "POST", host, "/api/device/wifi/scan", "application/json", `{}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"aps":[{"rssi":-48,"ssid":"home"}`) {
		t.Fatalf("scan: %d %s", w.Code, w.Body.String())
	}
	b.cfg.Device.Port = "none"
	if w := do(h, "POST", host, "/api/device/wifi/scan", "application/json", `{}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "屏幕未连接") {
		t.Fatalf("no screen: %d %s", w.Code, w.Body.String())
	}
}
