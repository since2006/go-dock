// Package app wires config, collectors, the hub, the device link and the
// web server into the go-dock daemon.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/since2006/go-dock/host/internal/autostart"
	"github.com/since2006/go-dock/host/internal/config"
	"github.com/since2006/go-dock/host/internal/device"
	"github.com/since2006/go-dock/host/internal/hub"
	"github.com/since2006/go-dock/host/internal/logx"
	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/mqtt"
	"github.com/since2006/go-dock/host/internal/plugins"
	"github.com/since2006/go-dock/host/internal/plugins/lights"
	"github.com/since2006/go-dock/host/internal/providers"
	"github.com/since2006/go-dock/host/internal/providers/claude"
	"github.com/since2006/go-dock/host/internal/providers/codex"
	"github.com/since2006/go-dock/host/internal/providers/pricing"
	"github.com/since2006/go-dock/host/internal/selfexe"
	"github.com/since2006/go-dock/host/internal/update"
	"github.com/since2006/go-dock/host/internal/web"
)

type Options struct {
	Version     string
	OpenBrowser bool      // false disables the configured browser launch
	Console     io.Writer // also log here; nil for file only
	// Restarted is set when the previous process exec'd or spawned this one
	// after an update; it may still be releasing the listen port.
	Restarted bool
}

// ErrRestart is returned by Run after an update was installed; the caller
// should start the new executable.
var ErrRestart = errors.New("restart requested")

// App implements web.Backend.
type App struct {
	version string
	store   *config.Store
	log     *logx.Logger
	hub     *hub.Hub
	device  *device.Manager
	updater *update.Updater
	mqtt    *mqtt.Pool
	lights  *lights.Plugin
	plugins *plugins.Host
	listen  string // loopback-reachable address hooks call back to

	ctx        context.Context
	restart    func()
	restarting atomic.Bool

	// upgradeMu keeps a program update and a firmware update from starting
	// together; hostUpdating stays set until the restart.
	upgradeMu    sync.Mutex
	hostUpdating atomic.Bool

	mu         sync.Mutex
	claude     *claude.Provider
	codex      *codex.Provider
	stopClaude context.CancelFunc
	stopCodex  context.CancelFunc
	claudeHome string
	codexHome  string
}

// Run starts the daemon and blocks until ctx ends.
func Run(ctx context.Context, o Options) error {
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	store, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		return err
	}
	cfg := store.Get()
	lg := logx.New(filepath.Join(dir, "logs", "go-dock.log"), o.Console, logx.ParseLevel(cfg.Log.Level))
	defer lg.Close()

	ln, err := listen(cfg.Web.Listen, o.Restarted)
	if err != nil {
		return fmt.Errorf("监听 %s 失败（是否已有 go-dock 在运行？）: %w", cfg.Web.Listen, err)
	}
	update.Cleanup()

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	a := &App{
		version: o.Version,
		store:   store,
		log:     lg,
		hub:     hub.New(hubOptions(cfg)),
		listen:  browserAddr(ln.Addr().String()),
		ctx:     ctx,
	}
	a.restart = func() { a.restarting.Store(true); stop() }
	a.device = device.NewManager(a.hub, deviceOptions(cfg), o.Version, lg.Logger, a.hub.Touch)
	a.updater = update.New(o.Version, lg.Logger, a.hub.Touch)
	a.updater.SetBeta(cfg.Update.Prerelease)
	a.mqtt = mqtt.NewPool(lg.Logger, a.hub.Touch)
	a.mqtt.SetOptions(mqttOptions(cfg))
	a.lights = lights.New(func(id string) lights.Bus { return a.mqtt.Client(id) }, lg.Logger, a.hub.Touch)
	a.lights.SetOptions(lightsOptions(cfg))
	a.plugins = plugins.New(a.lights)
	a.device.SetPages(a.plugins)
	a.applyProviders(cfg)
	store.OnChange(a.onConfigChange)

	url := "http://" + browserAddr(ln.Addr().String())
	lg.Info("go-dock 启动", "version", o.Version, "管理台", url, "config", store.Path())

	go a.hub.Run(ctx)
	go a.device.Run(ctx)
	go a.mqtt.Run(ctx)
	go a.updater.Run(ctx, func() bool { return a.store.Get().Update.AutoCheck })
	if o.OpenBrowser && !o.Restarted && cfg.Web.OpenBrowser {
		openBrowser(url)
	}
	srv := web.New(cfg.Web.Listen, a, lg.Ring, lg.Logger, o.Version)
	err = srv.Serve(ctx, ln)
	a.mu.Lock()
	for _, stop := range []context.CancelFunc{a.stopClaude, a.stopCodex} {
		if stop != nil {
			stop()
		}
	}
	a.mu.Unlock()
	if a.restarting.Load() {
		lg.Info("go-dock 正在重启")
		return ErrRestart
	}
	lg.Info("go-dock 已退出")
	return err
}

// listen binds addr. After a restart the old process may hold the port for
// a moment, so it retries for a few seconds.
func listen(addr string, retry bool) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	for i := 0; err != nil && retry && i < 40; i++ {
		time.Sleep(250 * time.Millisecond)
		ln, err = net.Listen("tcp", addr)
	}
	return ln, err
}

func hubOptions(c config.Config) hub.Options {
	return hub.Options{DoneTTL: c.Display.DoneTTL.D(), StaleAfter: c.Display.StaleAfter.D()}
}

func deviceOptions(c config.Config) device.Options {
	return device.Options{Enabled: c.Device.Enabled, Port: c.Device.Port, MaxSessions: c.Display.MaxSessions, ShowIdle: c.Display.ShowIdle}
}

func mqttOptions(c config.Config) []mqtt.BrokerOptions {
	var bs []mqtt.BrokerOptions
	for _, b := range c.MQTT.Brokers {
		url, _ := config.BrokerURL(b.URL) // validated; empty stays off
		bs = append(bs, mqtt.BrokerOptions{ID: b.ID, Name: b.Label(), Options: mqtt.Options{
			Enabled: b.Enabled, Broker: url, Username: b.Username, Password: b.Password, ClientID: b.ClientID,
		}})
	}
	return bs
}

func lightsOptions(c config.Config) lights.Options {
	o := lights.Options{Enabled: c.Plugins.Lights.Enabled, Brokers: map[string]string{}}
	for _, b := range c.MQTT.Brokers {
		o.Brokers[b.ID] = b.Label()
	}
	for _, l := range c.Plugins.Lights.Lights {
		l.Broker = c.MQTT.BrokerFor(l.Broker)
		o.Lights = append(o.Lights, l)
	}
	return o
}

func codexOptions(c config.Config) codex.Options {
	return codex.Options{AppServerPoll: c.Codex.AppServerPoll, AppServerInterval: c.Codex.AppServerInterval.D()}
}

// applyProviders starts, stops or restarts collectors to match cfg.
func (a *App) applyProviders(cfg config.Config) {
	a.mu.Lock()
	defer a.mu.Unlock()
	prices := pricing.New(cfg.Pricing)

	home := claude.Home(cfg.Claude.Home)
	if a.claude != nil && (!cfg.Claude.Enabled || home != a.claudeHome) {
		a.stopClaude()
		a.claude, a.stopClaude = nil, nil
		a.hub.RemoveProvider(model.ProviderClaude)
	}
	if cfg.Claude.Enabled && a.claude == nil {
		a.claudeHome = home
		a.claude = claude.New(home, a.hub, prices, a.log.Logger)
		ctx, cancel := context.WithCancel(a.ctx)
		a.stopClaude = cancel
		go a.claude.Run(ctx)
	} else if a.claude != nil {
		a.claude.SetPrices(prices)
	}

	home = codex.Home(cfg.Codex.Home)
	if a.codex != nil && (!cfg.Codex.Enabled || home != a.codexHome) {
		a.stopCodex()
		a.codex, a.stopCodex = nil, nil
		a.hub.RemoveProvider(model.ProviderCodex)
	}
	if cfg.Codex.Enabled && a.codex == nil {
		a.codexHome = home
		a.codex = codex.New(home, a.hub, codexOptions(cfg), a.log.Logger)
		ctx, cancel := context.WithCancel(a.ctx)
		a.stopCodex = cancel
		go a.codex.Run(ctx)
	} else if a.codex != nil {
		a.codex.SetOptions(codexOptions(cfg))
	}
}

func (a *App) onConfigChange(old, cur config.Config) {
	a.log.Level.Set(logx.ParseLevel(cur.Log.Level))
	a.hub.SetOptions(hubOptions(cur))
	a.device.SetOptions(deviceOptions(cur))
	a.mqtt.SetOptions(mqttOptions(cur))
	a.lights.SetOptions(lightsOptions(cur))
	a.applyProviders(cur)
	if old.Update.Prerelease != cur.Update.Prerelease {
		a.updater.SetBeta(cur.Update.Prerelease)
		go a.updater.Check(a.ctx) // show the right latest version right away
	}
	if old.Web.Listen != cur.Web.Listen {
		a.log.Warn("监听地址修改将在重启 go-dock 后生效，届时请重新安装集成", "listen", cur.Web.Listen)
	}
}

// ---- web.Backend ----

func (a *App) Snapshot() model.Snapshot             { return a.hub.Snapshot() }
func (a *App) Subscribe() (<-chan struct{}, func()) { return a.hub.Subscribe() }
func (a *App) Config() config.Config                { return a.store.Get() }
func (a *App) SetConfig(c config.Config) error      { return a.store.Set(c) }
func (a *App) DeviceStatus() any                    { return a.deviceStatus() }
func (a *App) ReconnectDevice()                     { a.device.Reconnect() }
func (a *App) DevicePorts() (any, error)            { return device.ListPorts() }

func (a *App) UpdateStatus() any { return a.updater.Status() }

func (a *App) CheckUpdate() error { return a.updater.Check(a.ctx) }

// ApplyUpdate validates synchronously, then downloads in the background;
// progress shows up in UpdateStatus and the process restarts when done.
func (a *App) ApplyUpdate() error {
	a.upgradeMu.Lock()
	defer a.upgradeMu.Unlock()
	switch st := a.updater.Status(); {
	case st.Dev:
		return errors.New("开发版本不支持在线升级，请下载发布版本")
	case !st.Available:
		return errors.New("没有可用的新版本")
	case st.State == "downloading" || st.State == "restarting":
		return errors.New("正在升级，请稍候")
	case a.device.Status().Flash.Busy():
		return errors.New("设备固件正在升级，请等它完成")
	}
	a.hostUpdating.Store(true)
	go func() {
		if err := a.updater.Apply(a.ctx); err != nil {
			a.hostUpdating.Store(false)
			return
		}
		time.Sleep(500 * time.Millisecond) // let the UI see "restarting"
		a.restart()
	}()
	return nil
}

type systemInfo struct {
	Version   string           `json:"version"`
	Exe       string           `json:"exe"`
	Autostart autostart.Status `json:"autostart"`
	Update    update.Status    `json:"update"`
}

func (a *App) System() any {
	exe, err := selfexe.Path()
	info := systemInfo{Version: a.version, Exe: exe, Autostart: autostart.Get(exe), Update: a.updater.Status()}
	if err != nil {
		info.Autostart.Error = err.Error()
	}
	return info
}

func (a *App) SetAutostart(enabled bool) error {
	if !enabled {
		err := autostart.Disable()
		if err == nil {
			a.log.Info("已关闭开机自启")
		}
		return err
	}
	exe, err := selfexe.Path()
	if err != nil {
		return err
	}
	if err := autostart.Enable(exe); err != nil {
		return err
	}
	a.log.Info("已开启开机自启", "exe", exe)
	return nil
}

func (a *App) DeviceFrame() any {
	return device.BuildFrame(a.hub.Snapshot(), deviceOptions(a.store.Get()), 0)
}

func (a *App) DevicePages() any { return device.BuildPages(a.plugins.Pages()) }

func (a *App) MQTTStatus() any { return a.mqtt.Status() }

func (a *App) PluginAction(ctx context.Context, act model.Action) error {
	return a.plugins.Do(ctx, act)
}

var (
	_ lights.Bus        = (*mqtt.Client)(nil)
	_ device.PageSource = (*plugins.Host)(nil)
)

func (a *App) Ingest(provider, kind string, body []byte) error {
	a.mu.Lock()
	cl, cx := a.claude, a.codex
	a.mu.Unlock()
	switch provider + "/" + kind {
	case "claude/hook":
		if cl != nil {
			return cl.HandleHook(body)
		}
	case "claude/statusline":
		if cl != nil {
			return cl.HandleStatusline(body)
		}
	case "codex/hook":
		if cx != nil {
			return cx.HandleHook(body)
		}
	default:
		return fmt.Errorf("unknown ingest %s/%s", provider, kind)
	}
	return nil // provider disabled: drop silently
}

func (a *App) homes() (string, string) {
	cfg := a.store.Get()
	return claude.Home(cfg.Claude.Home), codex.Home(cfg.Codex.Home)
}

func (a *App) Integrations() []providers.Status {
	t, terr := providers.CurrentTarget(a.listen)
	ch, xh := a.homes()
	list := []providers.Status{
		claude.HooksStatus(ch, t),
		claude.StatuslineStatus(ch, t),
		codex.HooksStatus(xh, t),
	}
	if terr != nil {
		for i := range list {
			if list[i].Error == "" {
				list[i].Error = terr.Error()
			}
		}
	}
	return list
}

func (a *App) Integration(name, action string) error {
	ch, xh := a.homes()
	if action == "uninstall" {
		switch name {
		case "claude-hooks":
			return claude.UninstallHooks(ch)
		case "claude-statusline":
			return claude.UninstallStatusline(ch)
		case "codex-hooks":
			return codex.UninstallHooks(xh)
		}
		return errors.New("未知集成 " + name)
	}
	t, err := providers.CurrentTarget(a.listen)
	if err != nil {
		return err
	}
	switch name {
	case "claude-hooks":
		return claude.InstallHooks(ch, t)
	case "claude-statusline":
		return claude.InstallStatusline(ch, t)
	case "codex-hooks":
		return codex.InstallHooks(xh, t)
	}
	return errors.New("未知集成 " + name)
}

// browserAddr turns a wildcard listen address into one a browser can open.
func browserAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		slog.Debug("打开浏览器失败", "err", err)
		return
	}
	go cmd.Wait()
}
