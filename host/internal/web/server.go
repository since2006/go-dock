// Package web serves the management UI and its JSON API.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/since2006/go-dock/host/internal/config"
	"github.com/since2006/go-dock/host/internal/ingest"
	"github.com/since2006/go-dock/host/internal/logx"
	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/plugins"
	"github.com/since2006/go-dock/host/internal/providers"
)

//go:embed static
var staticFS embed.FS

// Backend is what the API needs from the rest of the daemon.
type Backend interface {
	Snapshot() model.Snapshot
	Subscribe() (<-chan struct{}, func())
	Config() config.Config
	SetConfig(config.Config) error
	Integrations() []providers.Status
	Integration(name, action string) error
	Ingest(provider, kind string, body []byte) error
	DeviceStatus() any
	DeviceFrame() any
	// DevicePages is the pages message the device gets now.
	DevicePages() any
	DevicePorts() (any, error)
	// DeviceSounds lists the built-in alert sounds. SoundNotes gives the
	// notes of one, or of custom notes as written in the settings; DeviceBeep
	// plays notes on the device to try them out.
	DeviceSounds() any
	SoundNotes(sound, custom string) ([][2]int, error)
	DeviceBeep(notes [][2]int) error
	ReconnectDevice()
	ApplyFirmware() error
	UpdateStatus() any
	CheckUpdate() error
	ApplyUpdate() error
	System() any
	SetAutostart(enabled bool) error
	MQTTStatus() any
	// PluginAction handles a tap on a function page item, like the
	// device's action message.
	PluginAction(ctx context.Context, a model.Action) error
}

type Server struct {
	backend Backend
	logs    *logx.Ring
	log     *slog.Logger
	version string
	listen  string

	closing     chan struct{} // closed on shutdown so event streams end
	closingOnce sync.Once
}

func New(listen string, b Backend, logs *logx.Ring, log *slog.Logger, version string) *Server {
	return &Server{backend: b, logs: logs, log: log.With("component", "web"), version: version, listen: listen, closing: make(chan struct{})}
}

// Handler returns the full HTTP handler including security middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /", http.FileServerFS(static))

	mux.HandleFunc("GET /api/state", s.getState)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("GET /api/logs", s.getLogs)
	mux.HandleFunc("GET /api/config", s.getConfig)
	mux.HandleFunc("POST /api/config", s.postConfig)
	mux.HandleFunc("GET /api/integrations", s.getIntegrations)
	mux.HandleFunc("POST /api/integrations/{name}/{action}", s.postIntegration)
	mux.HandleFunc("GET /api/device/ports", s.getPorts)
	mux.HandleFunc("GET /api/device/frame", s.getFrame)
	mux.HandleFunc("GET /api/device/pages", s.getPages)
	mux.HandleFunc("GET /api/device/sounds", s.getSounds)
	mux.HandleFunc("POST /api/device/beep", s.postBeep)
	mux.HandleFunc("POST /api/device/notes", s.postNotes)
	mux.HandleFunc("POST /api/device/reconnect", s.postReconnect)
	mux.HandleFunc("POST /api/device/firmware", s.postFirmware)
	mux.HandleFunc("POST /api/ingest/{provider}/{kind}", s.postIngest)
	mux.HandleFunc("GET /api/system", s.getSystem)
	mux.HandleFunc("POST /api/autostart", s.postAutostart)
	mux.HandleFunc("POST /api/update/check", s.postUpdateCheck)
	mux.HandleFunc("POST /api/update/apply", s.postUpdateApply)
	mux.HandleFunc("POST /api/plugins/{page}/{item}", s.postPluginAction)
	return guard(s.listen, mux)
}

// Serve listens on the configured address until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	// Shutdown waits for active requests; event streams never finish on
	// their own, so end them first.
	srv.RegisterOnShutdown(func() { s.closingOnce.Do(func() { close(s.closing) }) })
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// guard rejects requests that could come from a web page the user happens
// to visit: wrong Host (DNS rebinding), cross-site fetches, and writes
// that are not JSON (which would skip the CORS preflight).
func guard(listen string, next http.Handler) http.Handler {
	loopback := isLoopbackListen(listen)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if loopback && !isLoopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "cross-site request", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			ct := r.Header.Get("Content-Type")
			if !strings.HasPrefix(strings.ToLower(ct), "application/json") {
				http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return true
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

type statePayload struct {
	Version  string         `json:"version"`
	Snapshot model.Snapshot `json:"snapshot"`
	Device   any            `json:"device"`
	Frame    any            `json:"frame"`
	Pages    any            `json:"pages"`
	MQTT     any            `json:"mqtt"`
	Update   any            `json:"update"`
}

func (s *Server) state() statePayload {
	return statePayload{
		Version:  s.version,
		Snapshot: s.backend.Snapshot(),
		Device:   s.backend.DeviceStatus(),
		Frame:    s.backend.DeviceFrame(),
		Pages:    s.backend.DevicePages(),
		MQTT:     s.backend.MQTTStatus(),
		Update:   s.backend.UpdateStatus(),
	}
}

func (s *Server) getState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.state())
}

func (s *Server) getLogs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 500
	}
	writeJSON(w, http.StatusOK, s.logs.Entries(logx.ParseLevel(r.URL.Query().Get("level")), limit))
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backend.Config())
}

func (s *Server) postConfig(w http.ResponseWriter, r *http.Request) {
	cur := s.backend.Config()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cur); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.backend.SetConfig(cur); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.log.Info("配置已更新")
	writeJSON(w, http.StatusOK, s.backend.Config())
}

func (s *Server) getIntegrations(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backend.Integrations())
}

func (s *Server) postIntegration(w http.ResponseWriter, r *http.Request) {
	name, action := r.PathValue("name"), r.PathValue("action")
	if action != "install" && action != "uninstall" {
		writeErr(w, http.StatusNotFound, errors.New("unknown action"))
		return
	}
	if err := s.backend.Integration(name, action); err != nil {
		s.log.Warn("集成操作失败", "name", name, "action", action, "err", err)
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.log.Info("集成操作完成", "name", name, "action", action)
	writeJSON(w, http.StatusOK, s.backend.Integrations())
}

func (s *Server) getPorts(w http.ResponseWriter, r *http.Request) {
	ports, err := s.backend.DevicePorts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, ports)
}

func (s *Server) getFrame(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backend.DeviceFrame())
}

func (s *Server) getSounds(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backend.DeviceSounds())
}

// soundNotes reads {"sound": id, "custom": notes for sound "custom"} and
// resolves it to notes; on an error it has answered the request.
func (s *Server) soundNotes(w http.ResponseWriter, r *http.Request) ([][2]int, bool) {
	var body struct {
		Sound  string `json:"sound"`
		Custom string `json:"custom"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return nil, false
	}
	notes, err := s.backend.SoundNotes(body.Sound, body.Custom)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return nil, false
	}
	return notes, true
}

// postNotes gives the notes of a sound without playing it, for the
// browser to play.
func (s *Server) postNotes(w http.ResponseWriter, r *http.Request) {
	if notes, ok := s.soundNotes(w, r); ok {
		writeJSON(w, http.StatusOK, map[string]any{"notes": notes})
	}
}

// postBeep plays a sound on the device; 409 means the device cannot play
// it now (not connected, firmware without a buzzer).
func (s *Server) postBeep(w http.ResponseWriter, r *http.Request) {
	notes, ok := s.soundNotes(w, r)
	if !ok {
		return
	}
	// The notes come back either way, so the browser can play them when
	// the device cannot.
	if err := s.backend.DeviceBeep(notes); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "notes": notes})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "notes": notes})
}

func (s *Server) postReconnect(w http.ResponseWriter, r *http.Request) {
	s.backend.ReconnectDevice()
	writeJSON(w, http.StatusOK, s.backend.DeviceStatus())
}

// postFirmware starts a firmware update; progress shows up in the device
// status.
func (s *Server) postFirmware(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.ApplyFirmware(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.backend.DeviceStatus())
}

func (s *Server) getSystem(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backend.System())
}

func (s *Server) postAutostart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.backend.SetAutostart(req.Enabled); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.backend.System())
}

func (s *Server) postUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.CheckUpdate(); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, s.backend.System())
}

func (s *Server) postUpdateApply(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.ApplyUpdate(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.backend.UpdateStatus())
}

func (s *Server) getPages(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backend.DevicePages())
}

// postPluginAction takes {"act":"on|off|toggle"} and answers with the
// pages message after the tap.
func (s *Server) postPluginAction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Act string `json:"act"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	err := s.backend.PluginAction(ctx, model.Action{Page: r.PathValue("page"), Item: r.PathValue("item"), Act: body.Act})
	switch {
	case errors.Is(err, plugins.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, plugins.ErrBadAct):
		writeErr(w, http.StatusBadRequest, err)
	case err != nil:
		writeErr(w, http.StatusBadGateway, err)
	default:
		writeJSON(w, http.StatusOK, s.backend.DevicePages())
	}
}

// postIngest always answers with an empty body: hook runners may treat
// output as a decision.
func (s *Server) postIngest(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, ingest.MaxBody))
	if err != nil {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	provider, kind := r.PathValue("provider"), r.PathValue("kind")
	if err := s.backend.Ingest(provider, kind, body); err != nil {
		// Payloads may hold prompts; log only the error.
		s.log.Warn("忽略无法处理的上报", "provider", provider, "kind", kind, "err", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.log.Debug("收到上报", "provider", provider, "kind", kind, "bytes", len(body))
	w.WriteHeader(http.StatusOK)
}

// events streams "state" and "log" server-sent events.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	changes, cancelState := s.backend.Subscribe()
	defer cancelState()
	logs, cancelLogs := s.logs.Subscribe()
	defer cancelLogs()

	send := func(event string, v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return true
		}
		if _, err := io.WriteString(w, "event: "+event+"\ndata: "+string(b)+"\n\n"); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send("state", s.state()) {
		return
	}

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	// The device status and countdowns move without hub changes, so state
	// is also pushed periodically.
	periodic := time.NewTicker(5 * time.Second)
	defer periodic.Stop()
	var throttle <-chan time.Time
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.closing:
			return
		case <-changes:
			if throttle == nil {
				throttle = time.After(250 * time.Millisecond)
			}
		case <-throttle:
			throttle = nil
			if !send("state", s.state()) {
				return
			}
		case <-periodic.C:
			if !send("state", s.state()) {
				return
			}
		case e := <-logs:
			if !send("log", e) {
				return
			}
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}
