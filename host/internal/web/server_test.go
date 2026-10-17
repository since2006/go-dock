package web

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/since2006/go-dock/host/internal/config"
	"github.com/since2006/go-dock/host/internal/logx"
	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/providers"
)

type fakeBackend struct {
	cfg       config.Config
	ingests   []string
	autostart bool
}

func (f *fakeBackend) Snapshot() model.Snapshot             { return model.Snapshot{} }
func (f *fakeBackend) Subscribe() (<-chan struct{}, func()) { return make(chan struct{}), func() {} }
func (f *fakeBackend) Config() config.Config                { return f.cfg }
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
func (f *fakeBackend) UpdateStatus() any         { return map[string]string{"state": "idle"} }
func (f *fakeBackend) CheckUpdate() error        { return nil }
func (f *fakeBackend) ApplyUpdate() error        { return errors.New("没有可用的新版本") }
func (f *fakeBackend) System() any               { return map[string]any{"autostart": f.autostart} }
func (f *fakeBackend) SetAutostart(on bool) error {
	f.autostart = on
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

func TestConfigPut(t *testing.T) {
	b, h := newTestServer("127.0.0.1:47321")
	w := do(h, "PUT", "127.0.0.1:47321", "/api/config", "application/json", `{"display":{"max_sessions":8,"done_ttl":"10m"}}`)
	if w.Code != http.StatusOK || b.cfg.Display.MaxSessions != 8 || b.cfg.Display.DoneTTL.D().Minutes() != 10 {
		t.Fatalf("put: %d %s %+v", w.Code, w.Body.String(), b.cfg.Display)
	}
	if b.cfg.Web.Listen != config.DefaultListen {
		t.Fatal("partial put cleared other fields")
	}
	if w := do(h, "PUT", "127.0.0.1:47321", "/api/config", "application/json", `{"display":{"max_sessions":0}}`); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid put: %d", w.Code)
	}
	if w := do(h, "PUT", "127.0.0.1:47321", "/api/config", "application/json", `{"nope":1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", w.Code)
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
}
