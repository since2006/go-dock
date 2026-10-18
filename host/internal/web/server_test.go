package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/since2006/go-dock/host/internal/config"
	"github.com/since2006/go-dock/host/internal/logx"
	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/plugins"
	"github.com/since2006/go-dock/host/internal/providers"
)

type fakeBackend struct {
	cfg       config.Config
	ingests   []string
	autostart bool
	flashing  bool
	actions   []model.Action
	beeps     []string
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
func (f *fakeBackend) DevicePages() any { return map[string]any{"t": "pages", "n": len(f.actions)} }
func (f *fakeBackend) MQTTStatus() any  { return map[string]string{"state": "disabled"} }
func (f *fakeBackend) DeviceSounds() any {
	return []map[string]string{{"id": "chime", "name": "叮咚"}}
}
func (f *fakeBackend) DeviceBeep(sound string) error {
	if sound == "" {
		return errors.New("设备未连接")
	}
	f.beeps = append(f.beeps, sound)
	return nil
}
func (f *fakeBackend) PluginAction(ctx context.Context, a model.Action) error {
	switch {
	case a.Page == "aircon" && a.Item == "temp" && (a.Act == model.ActInc || a.Act == model.ActDec):
	case a.Page != "lights" || a.Item != "main":
		return plugins.ErrNotFound
	case a.Act != model.ActOn && a.Act != model.ActOff && a.Act != model.ActToggle:
		return plugins.ErrBadAct
	case a.Act == model.ActOff:
		return errors.New("MQTT 未连接")
	}
	f.actions = append(f.actions, a)
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

func TestConfigSaveAircon(t *testing.T) {
	b, h := newTestServer("127.0.0.1:47321")
	host := "127.0.0.1:47321"
	if w := do(h, "POST", host, "/api/config", "application/json", `{"plugins":{"aircon":{"enabled":true,"name":"会议室","model":"ybofb"}}}`); w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if a := b.cfg.Plugins.Aircon; !a.Enabled || a.Name != "会议室" || a.Model != "ybofb" || b.cfg.Plugins.Lights.Lights == nil {
		t.Fatalf("aircon %+v", b.cfg.Plugins)
	}
	if w := do(h, "POST", host, "/api/config", "application/json", `{"plugins":{"aircon":{"model":"nope"}}}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad model: %d", w.Code)
	}
	// The stepper's acts reach the plugin as they are.
	if w := do(h, "POST", host, "/api/plugins/aircon/temp", "application/json", `{"act":"inc"}`); w.Code != http.StatusOK {
		t.Fatalf("inc: %d %s", w.Code, w.Body.String())
	}
	if n := len(b.actions); n != 1 || b.actions[0] != (model.Action{Page: "aircon", Item: "temp", Act: "inc"}) {
		t.Fatalf("actions %+v", b.actions)
	}
}

func TestConfigSaveLights(t *testing.T) {
	b, h := newTestServer("127.0.0.1:47321")
	two := `{"plugins":{"lights":{"enabled":true,"lights":[
		{"id":"main","name":"主灯","command_topic":"a/set","state_topic":"a/state","state_path":"state"},
		{"id":"desk","name":"台灯","command_topic":"b/set"}]}}}`
	if w := do(h, "POST", "127.0.0.1:47321", "/api/config", "application/json", two); w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	// A shorter list whose first light omits keys replaces the whole list.
	one := `{"plugins":{"lights":{"lights":[{"id":"desk","name":"台灯","command_topic":"b/set"}]}}}`
	if w := do(h, "POST", "127.0.0.1:47321", "/api/config", "application/json", one); w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	ls := b.cfg.Plugins.Lights
	if !ls.Enabled || len(ls.Lights) != 1 || ls.Lights[0] != (config.Light{ID: "desk", Name: "台灯", CommandTopic: "b/set"}) {
		t.Fatalf("lights %+v", ls)
	}
	bad := `{"plugins":{"lights":{"lights":[{"id":"Main","name":"x","command_topic":"a"}]}}}`
	if w := do(h, "POST", "127.0.0.1:47321", "/api/config", "application/json", bad); w.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", w.Code)
	}
	if len(b.cfg.Plugins.Lights.Lights) != 1 || b.cfg.Plugins.Lights.Lights[0].ID != "desk" {
		t.Fatalf("rejected save changed the config: %+v", b.cfg.Plugins.Lights)
	}
}

func TestPluginAction(t *testing.T) {
	b, h := newTestServer("127.0.0.1:47321")
	host := "127.0.0.1:47321"
	w := do(h, "POST", host, "/api/plugins/lights/main", "application/json", `{"act":"toggle"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"n":1`) {
		t.Fatalf("toggle: %d %s", w.Code, w.Body.String())
	}
	if len(b.actions) != 1 || b.actions[0] != (model.Action{Page: "lights", Item: "main", Act: "toggle"}) {
		t.Fatalf("actions %+v", b.actions)
	}
	for _, tc := range []struct {
		path, body string
		code       int
	}{
		{"/api/plugins/lights/nope", `{"act":"on"}`, http.StatusNotFound},
		{"/api/plugins/lights/main", `{"act":"dim"}`, http.StatusBadRequest},
		{"/api/plugins/lights/main", `not json`, http.StatusBadRequest},
		{"/api/plugins/lights/main", `{"act":"off"}`, http.StatusBadGateway},
	} {
		if w := do(h, "POST", host, tc.path, "application/json", tc.body); w.Code != tc.code {
			t.Errorf("%s %s: %d, want %d", tc.path, tc.body, w.Code, tc.code)
		}
	}
	if w := do(h, "POST", host, "/api/plugins/lights/main", "text/plain", `{"act":"on"}`); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain: %d", w.Code)
	}
	w = do(h, "GET", host, "/api/state", "", "")
	if !strings.Contains(w.Body.String(), `"pages":{`) || !strings.Contains(w.Body.String(), `"mqtt":{"state":"disabled"}`) {
		t.Fatalf("state: %s", w.Body.String())
	}
}

func TestBeep(t *testing.T) {
	b, h := newTestServer("127.0.0.1:47321")
	host := "127.0.0.1:47321"
	if w := do(h, "GET", host, "/api/device/sounds", "", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"chime"`) {
		t.Fatalf("sounds: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "POST", host, "/api/device/beep", "application/json", `{"sound":"rise"}`); w.Code != http.StatusOK || len(b.beeps) != 1 || b.beeps[0] != "rise" {
		t.Fatalf("beep: %d %s %v", w.Code, w.Body.String(), b.beeps)
	}
	if w := do(h, "POST", host, "/api/device/beep", "application/json", `{}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "设备未连接") {
		t.Fatalf("no device: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "POST", host, "/api/device/beep", "application/json", `nope`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad body: %d", w.Code)
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
