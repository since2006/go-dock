package device

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/since2006/go-dock/host/internal/logx"
	"github.com/since2006/go-dock/host/internal/model"
)

// Options configures the links to the screen.
type Options struct {
	Enabled bool
	// Addr ("host" or "host:port") and Code (the pairing code the screen
	// shows) reach the screen over WiFi, which carries everything it shows.
	Addr string
	Code string
	// Port is the USB cable used to set up the screen's WiFi and update
	// its firmware: "auto" or a serial port path.
	Port string
	// Name and HostID tell the screen which computer this is.
	Name        string
	HostID      string
	MaxSessions int
	ShowIdle    bool
	// BeepNotes is what the device plays when a task starts waiting for the
	// user, DoneNotes when one finishes; none plays nothing.
	BeepNotes []Note
	DoneNotes []Note
	Volume    int // percent, for both

	// Brightness in percent; the screen dims to DimBrightness after
	// DimAfter without an active task (0: never). See EncodeSettings.
	Brightness    int
	DimBrightness int
	DimAfter      time.Duration
}

// SnapshotSource is what the manager mirrors to the device; *hub.Hub
// implements it.
type SnapshotSource interface {
	Snapshot() model.Snapshot
	Subscribe() (<-chan struct{}, func())
}

// LineSource provides the readings of the line on the Agent page;
// *plugins.Host implements it.
type LineSource interface {
	Line() []model.Reading
}

// Link states reported in Status.State and USBStatus.State.
const (
	StateDisabled  = "disabled"
	StateUnset     = "unset" // WiFi: no screen address or pairing code yet
	StateSearching = "searching"
	StateConnected = "connected" // linked, no device hello yet
	StateReady     = "ready"     // device answered hello
)

// Status is the link over WiFi, shown on the web UI, plus the USB cable.
type Status struct {
	State    string `json:"state"`
	Addr     string `json:"addr,omitempty"` // the screen's address
	Firmware string `json:"firmware,omitempty"`
	// Incompatible: the device speaks another protocol version; it gets no
	// snapshots, only firmware updates.
	Incompatible bool        `json:"incompatible,omitempty"`
	Width        int         `json:"width,omitempty"`
	Height       int         `json:"height,omitempty"`
	LastSent     time.Time   `json:"last_sent,omitzero"`
	LastHeard    time.Time   `json:"last_heard,omitzero"`
	Error        string      `json:"error,omitempty"`
	Sent         uint64      `json:"sent"`
	USB          USBStatus   `json:"usb"`
	Flash        FlashStatus `json:"flash,omitzero"` // kept across reconnects
}

// USBStatus is the cable that sets up the screen's WiFi and updates its
// firmware; nothing it shows goes over it.
type USBStatus struct {
	State    string `json:"state"`
	Port     string `json:"port,omitempty"`
	Firmware string `json:"firmware,omitempty"`
	// Net is what the screen last said about its WiFi.
	Net          *NetStatus `json:"net,omitempty"`
	Incompatible bool       `json:"incompatible,omitempty"`
	Error        string     `json:"error,omitempty"`
}

// Manager keeps the links to the screen up: over WiFi it pushes snapshots
// and the line, over USB it sets up the WiFi and updates the firmware.
type Manager struct {
	src      SnapshotSource
	log      *slog.Logger
	onChange func()

	// Replaceable in tests.
	list       func() ([]PortInfo, error)
	open       func(name string) (io.ReadWriteCloser, error)
	dial       func(addr string) (io.ReadWriteCloser, error)
	heartbeat  time.Duration
	helloEvery time.Duration
	rescan     time.Duration // USB: looking for the cable again
	// WiFi: how long to wait before dialing again, after the screen turned
	// go-dock away, and without a word from the screen.
	netRetry, deniedWait, netSilence time.Duration
	debounce                         time.Duration
	writeTimeout                     time.Duration
	// Firmware update: waits for the user to answer on the screen
	// (otaConfirm, WiFi only), for the device to erase (otaReady), write a
	// chunk (otaAck) and check the image (otaDone); for the link's loop to
	// take the image (otaPickup); and for the new firmware to say hello
	// (otaReboot), ignoring the old one for otaSettle after it accepted.
	otaConfirm, otaReady, otaAck, otaDone, otaPickup, otaReboot, otaSettle time.Duration

	kick     chan struct{} // reconnect over WiFi
	usbKick  chan struct{} // reopen the USB cable
	usbFlash chan flashJob // an update for the USB loop
	netFlash chan flashJob // and for the WiFi loop
	beepReq  chan []byte   // the beep line of a sound to try out
	wifiReq  chan []byte   // a wifi line, taken only by a USB link that can send it now
	scanReq  chan []byte   // a wifi_scan line, likewise
	scanRes  chan scanResult
	scanMu   sync.Mutex // one scan at a time
	// scanTimeout bounds a scan: handing the request over, then the answer.
	scanTimeout time.Duration
	settings    chan struct{} // the brightness settings changed
	// waiting and active hold waitingTasks and activeTasks as of the last
	// snapshot sent to a device that beeps; only the WiFi loop uses them.
	// They outlive connections, so reconnecting does not beep again for
	// the same tasks.
	waiting, active map[string]bool

	mu          sync.Mutex
	line        LineSource
	opts        Options
	status      Status
	seq         uint64
	flashDoneAt time.Time
}

// NewManager creates the link manager. onChange, if set, is called when a
// firmware update makes progress or the USB cable reports news.
func NewManager(src SnapshotSource, opts Options, log *slog.Logger, onChange func()) *Manager {
	return &Manager{
		src:          src,
		log:          log.With("component", "device"),
		onChange:     onChange,
		list:         ListPorts,
		open:         openSerial,
		dial:         dialNet,
		heartbeat:    5 * time.Second,
		helloEvery:   5 * time.Second,
		rescan:       2 * time.Second,
		netRetry:     5 * time.Second,
		deniedWait:   10 * time.Second,
		netSilence:   20 * time.Second,
		debounce:     200 * time.Millisecond,
		writeTimeout: 10 * time.Second,
		otaConfirm:   75 * time.Second, // the screen gives up after 60
		otaReady:     60 * time.Second,
		otaAck:       10 * time.Second,
		otaDone:      30 * time.Second,
		otaPickup:    5 * time.Second,
		otaReboot:    60 * time.Second,
		otaSettle:    5 * time.Second,
		kick:         make(chan struct{}, 1),
		usbKick:      make(chan struct{}, 1),
		usbFlash:     make(chan flashJob),
		netFlash:     make(chan flashJob),
		beepReq:      make(chan []byte, 1),
		wifiReq:      make(chan []byte),
		scanReq:      make(chan []byte),
		scanRes:      make(chan scanResult, 1),
		scanTimeout:  15 * time.Second,
		settings:     make(chan struct{}, 1),
		opts:         opts,
		status:       Status{State: StateSearching, USB: USBStatus{State: StateSearching}},
	}
}

// SetOptions applies new options, reconnecting the links whose settings
// changed.
func (m *Manager) SetOptions(o Options) {
	m.mu.Lock()
	old := m.opts
	m.opts = o
	m.mu.Unlock()
	if old.Enabled != o.Enabled || old.Addr != o.Addr || old.Code != o.Code || old.Name != o.Name || old.HostID != o.HostID {
		poke(m.kick)
	}
	if old.Enabled != o.Enabled || old.Port != o.Port {
		poke(m.usbKick)
	}
	if !sameSettings(old, o) {
		poke(m.settings)
	}
}

func poke(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// SetLine sets where the line's readings come from; call it before Run.
func (m *Manager) SetLine(l LineSource) {
	m.mu.Lock()
	m.line = l
	m.mu.Unlock()
}

func (m *Manager) lineSource() LineSource {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.line
}

// Reconnect drops both links and connects again immediately.
func (m *Manager) Reconnect() {
	poke(m.kick)
	poke(m.usbKick)
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *Manager) options() Options {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opts
}

func (m *Manager) update(f func(s *Status)) {
	m.mu.Lock()
	f(&m.status)
	m.mu.Unlock()
}

// updateUSB changes the cable's status, telling the web UI when it moved.
func (m *Manager) updateUSB(f func(u *USBStatus)) {
	m.mu.Lock()
	old := m.status.USB
	f(&m.status.USB)
	moved := m.status.USB != old
	m.mu.Unlock()
	if moved {
		m.changed()
	}
}

// setError records err, logging it only when it changes to avoid a line
// every retry.
func (m *Manager) setError(err error) {
	msg := err.Error()
	m.mu.Lock()
	changed := m.status.Error != msg
	m.status.Error = msg
	m.mu.Unlock()
	if changed {
		m.log.Warn("屏幕连接问题", "err", msg)
	}
}

// Run keeps both links up until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { m.runNet(ctx) })
	wg.Go(func() { m.runUSB(ctx) })
	wg.Wait()
}

// runNet keeps the WiFi link to the screen up.
func (m *Manager) runNet(ctx context.Context) {
	for ctx.Err() == nil {
		opts := m.options()
		switch {
		case !opts.Enabled:
			m.update(func(s *Status) { netUnlinked(s); s.State, s.Error = StateDisabled, "" })
			m.wait(ctx, m.kick, 0)
			continue
		case opts.Addr == "" || opts.Code == "":
			m.update(func(s *Status) { netUnlinked(s); s.State, s.Error = StateUnset, "" })
			m.wait(ctx, m.kick, 0)
			continue
		}
		m.update(netUnlinked)
		addr, err := NetAddr(opts.Addr)
		if err != nil {
			m.setError(err)
			m.wait(ctx, m.kick, 0) // only a new address helps
			continue
		}
		conn, err := m.dial(addr)
		if err != nil {
			m.setError(dialError(addr, err))
			m.wait(ctx, m.kick, m.netRetry)
			continue
		}
		retry := m.serveNet(ctx, addr, conn)
		conn.Close()
		m.update(netUnlinked)
		if retry > 0 {
			m.wait(ctx, m.kick, retry)
		}
	}
}

// dialError says in plain words why the screen could not be reached.
func dialError(addr string, err error) error {
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("屏幕 %s 拒绝连接：端口不对，或屏幕固件还不支持 WiFi", addr)
	case errors.As(err, &ne) && ne.Timeout():
		return fmt.Errorf("连不上屏幕 %s：屏幕没开、没连上 WiFi，或 IP 不对", addr)
	}
	return fmt.Errorf("连不上屏幕 %s: %w", addr, err)
}

// netUnlinked forgets the screen while connecting to it.
func netUnlinked(s *Status) {
	*s = Status{State: StateSearching, Error: s.Error, LastSent: s.LastSent, LastHeard: s.LastHeard, Sent: s.Sent, USB: s.USB, Flash: s.Flash}
}

// wait sleeps for d (forever when d is 0) or until kick.
func (m *Manager) wait(ctx context.Context, kick <-chan struct{}, d time.Duration) {
	var timer <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-ctx.Done():
	case <-kick:
	case <-timer:
	}
}

// serveNet runs one WiFi connection until it fails, ctx ends or a
// reconnect is requested. It returns how long to wait before dialing
// again: 0 after a reconnect request.
func (m *Manager) serveNet(ctx context.Context, addr string, conn io.ReadWriteCloser) time.Duration {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// A screen that turns go-dock away keeps its reason up until a
	// handshake succeeds.
	m.update(func(s *Status) { s.State, s.Addr = StateConnected, addr })
	// Dialed again every few seconds while the screen turns go-dock away:
	// the handshake is what gets logged.
	m.log.Debug("已连接屏幕", "addr", addr)
	defer m.log.Debug("与屏幕的连接已断开", "addr", addr)

	// A screen that lost power or WiFi goes quiet rather than closing: it
	// answers every snapshot, so silence ends the link.
	silence := time.NewTimer(m.netSilence)
	defer silence.Stop()

	msgs := make(chan DeviceMsg, 16)
	readErr := make(chan error, 1)
	go m.readLoop(sctx, conn, msgs, readErr)

	changes, unsubscribe := m.src.Subscribe()
	defer unsubscribe()

	heartbeat := time.NewTicker(m.heartbeat)
	defer heartbeat.Stop()
	hello := time.NewTicker(m.helloEvery)
	defer hello.Stop()
	var debounce <-chan time.Time

	// heard: the screen said hello on this link, so it takes updates, even
	// in another protocol (that is what fixes it); ready: in this one.
	ready, incompatible, heard := false, false, false
	fail := func(err error) time.Duration {
		m.setError(fmt.Errorf("%s: %w", addr, err))
		return m.netRetry
	}
	// The line of readings follows each snapshot; a debounced change
	// resends it only when it differs.
	var lastLine []byte
	sendLine := func(force bool) error {
		src := m.lineSource()
		if !ready || src == nil {
			return nil
		}
		line, err := EncodeLine(BuildLine(src.Line()))
		if err != nil || (!force && bytes.Equal(line, lastLine)) {
			return err
		}
		if err := m.write(conn, line); err != nil {
			return err
		}
		lastLine = line
		return nil
	}
	// push sends a snapshot, the line and a beep when a task started
	// waiting for the user.
	push := func(forceLine bool) error {
		snap, err := m.sendSnapshot(conn)
		if err != nil {
			return err
		}
		if err := sendLine(forceLine); err != nil {
			return err
		}
		if ready {
			return m.alert(conn, snap)
		}
		return nil
	}
	sayHello := func() error {
		o := m.options()
		return m.write(conn, EncodeHello(o.Name, o.HostID, o.Code))
	}
	if err := sayHello(); err != nil {
		return fail(err)
	}
	if err := push(true); err != nil {
		return fail(err)
	}
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-m.kick:
			return 0
		case err := <-readErr:
			return fail(err)
		case <-silence.C:
			return fail(fmt.Errorf("%d 秒没有收到屏幕的消息", int(m.netSilence/time.Second)))
		case msg := <-msgs:
			silence.Reset(m.netSilence)
			m.update(func(s *Status) { s.LastHeard = time.Now() })
			switch msg.T {
			case "hello":
				if m.flashed(msg.FW) {
					continue // the old firmware, about to restart
				}
				heard = true
				m.update(func(s *Status) { s.Firmware = msg.FW })
				if msg.Proto != ProtoVersion {
					incompatible, ready = true, false
					m.update(func(s *Status) { s.Incompatible = true })
					m.setError(fmt.Errorf("屏幕协议版本 %d 与主机 %d 不兼容，已停止发送；在网页上升级屏幕固件即可", msg.Proto, ProtoVersion))
					continue
				}
				incompatible = false
				if !ready {
					m.log.Info("屏幕握手完成", "addr", addr, "fw", msg.FW)
				}
				ready = true
				m.update(func(s *Status) {
					s.State, s.Incompatible, s.Width, s.Height, s.Error = StateReady, false, msg.W, msg.H, ""
				})
				if err := m.write(conn, EncodeSettings(m.options())); err != nil {
					return fail(err)
				}
				if err := push(true); err != nil {
					return fail(err)
				}
			case "denied":
				// The screen hangs up after saying why.
				reason := msg.Msg
				if reason == "" {
					reason = "屏幕拒绝了连接"
				}
				m.setError(errors.New(reason))
				return m.deniedWait
			case "ack":
			case "log":
				m.log.Log(sctx, logx.ParseLevel(msg.Level), msg.Msg, "src", "device")
			default:
				m.log.Debug("忽略屏幕消息", "t", msg.T)
			}
		case <-changes:
			if debounce == nil {
				debounce = time.After(m.debounce)
			}
		case <-debounce:
			debounce = nil
			if !incompatible {
				if err := push(false); err != nil {
					return fail(err)
				}
			}
		case <-heartbeat.C:
			// A screen in another protocol gets no data, only hellos: its
			// answers keep the link up for an update.
			var err error
			if incompatible {
				err = sayHello()
			} else {
				err = push(true)
			}
			if err != nil {
				return fail(err)
			}
		case <-m.settings:
			if ready {
				if err := m.write(conn, EncodeSettings(m.options())); err != nil {
					return fail(err)
				}
			}
		case line := <-m.beepReq:
			if ready {
				if err := m.write(conn, line); err != nil {
					return fail(err)
				}
			}
		case <-hello.C:
			if !ready && !incompatible {
				if err := sayHello(); err != nil {
					return fail(err)
				}
			}
		case job := <-m.netFlash:
			if !heard {
				m.flashFailed(errors.New("屏幕未就绪"))
				continue
			}
			restarting, err := m.flash(sctx, conn, msgs, readErr, m.kick, job)
			switch {
			case errors.Is(err, errKicked), ctx.Err() != nil:
				return 0
			case err != nil:
				return fail(err)
			case restarting:
				// The screen restarts and drops the link; its new firmware
				// says hello on the next one.
				return m.netRetry
			}
			// Nothing came or went during the update: the screen gets the
			// state again, and has its time to answer.
			silence.Reset(m.netSilence)
			if !incompatible {
				if err := push(true); err != nil {
					return fail(err)
				}
			}
		}
	}
}

// runUSB keeps the USB cable open while the screen is plugged in. Not
// finding it is the usual case, so that is no error.
func (m *Manager) runUSB(ctx context.Context) {
	for ctx.Err() == nil {
		opts := m.options()
		if !opts.Enabled {
			m.updateUSB(func(u *USBStatus) { *u = USBStatus{State: StateDisabled} })
			m.wait(ctx, m.usbKick, 0)
			continue
		}
		name, err := m.pick(opts.Port)
		if err != nil {
			m.updateUSB(func(u *USBStatus) { *u = USBStatus{State: StateSearching} })
			m.wait(ctx, m.usbKick, m.rescan)
			continue
		}
		conn, err := m.open(name)
		if err != nil {
			msg := fmt.Sprintf("打开 %s: %v", name, err)
			m.updateUSB(func(u *USBStatus) { *u = USBStatus{State: StateSearching, Error: msg} })
			m.wait(ctx, m.usbKick, m.rescan)
			continue
		}
		retry := m.serveUSB(ctx, name, conn)
		conn.Close()
		m.updateUSB(func(u *USBStatus) { *u = USBStatus{State: StateSearching, Error: u.Error} })
		if retry > 0 {
			m.wait(ctx, m.usbKick, retry)
		}
	}
}

// pick finds the screen's USB serial port.
func (m *Manager) pick(port string) (string, error) {
	if port != "" && port != "auto" {
		return port, nil
	}
	ports, err := m.list()
	if err != nil {
		return "", fmt.Errorf("列出串口: %w", err)
	}
	name := pickPort(ports)
	if name == "" {
		return "", errors.New("未找到屏幕（VID 303A）")
	}
	return name, nil
}

// serveUSB runs the USB cable until it fails, ctx ends or a reconnect is
// requested: hello, the WiFi set-up messages and firmware updates, nothing
// else. It returns how long to wait before opening it again.
func (m *Manager) serveUSB(ctx context.Context, name string, conn io.ReadWriteCloser) time.Duration {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	m.updateUSB(func(u *USBStatus) { *u = USBStatus{State: StateConnected, Port: name} })
	m.log.Info("屏幕 USB 已连接", "port", name)
	defer m.log.Info("屏幕 USB 已断开", "port", name)

	msgs := make(chan DeviceMsg, 16)
	readErr := make(chan error, 1)
	go m.readLoop(sctx, conn, msgs, readErr)

	hello := time.NewTicker(m.helloEvery)
	defer hello.Stop()
	// asked: a hello of ours awaits the screen's answer. A hello the screen
	// sends unasked (it restarted) gets ours in return: hearing go-dock is
	// what keeps a freshly updated firmware, which rolls back otherwise.
	ready, asked := false, false
	// Set-up requests are taken only once the screen said hello: one the
	// cable cannot carry now times out with its caller instead of vanishing.
	whenReady := func(c chan []byte) chan []byte {
		if ready {
			return c
		}
		return nil
	}
	fail := func(err error) time.Duration {
		msg := fmt.Sprintf("%s: %v", name, err)
		m.updateUSB(func(u *USBStatus) { u.Error = msg })
		return m.rescan
	}
	// No pairing code: the cable is not the network.
	sayHello := func() error {
		o := m.options()
		asked = true
		return m.write(conn, EncodeHello(o.Name, o.HostID, ""))
	}
	if err := sayHello(); err != nil {
		return fail(err)
	}
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-m.usbKick:
			return 0
		case err := <-readErr:
			return fail(err)
		case msg := <-msgs:
			switch msg.T {
			case "hello":
				if m.flashed(msg.FW) {
					continue // the old firmware, about to restart
				}
				if asked {
					asked = false // the answer to ours
				} else if err := sayHello(); err != nil {
					return fail(err)
				}
				// Updates work whatever the protocol: that is what fixes it.
				if !ready {
					m.log.Info("屏幕 USB 握手完成", "fw", msg.FW)
				}
				ready = true
				m.updateUSB(func(u *USBStatus) {
					u.State, u.Firmware, u.Incompatible, u.Error = StateReady, msg.FW, msg.Proto != ProtoVersion, ""
				})
			case "net":
				st := NetStatus{State: msg.State, SSID: msg.SSID, IP: msg.IP, Port: msg.Port, Code: msg.Code, Fixed: msg.Fixed, Err: msg.Err}
				m.updateUSB(func(u *USBStatus) { u.Net = &st })
			case "wifi_list":
				select {
				case m.scanRes <- scanResult{aps: msg.APs, err: msg.Err}:
				default: // nobody asked, or an answer is waiting already
				}
			case "log":
				m.log.Log(sctx, logx.ParseLevel(msg.Level), msg.Msg, "src", "device")
			case "ack":
			default:
				m.log.Debug("忽略屏幕 USB 消息", "t", msg.T)
			}
		case line := <-whenReady(m.scanReq):
			if err := m.write(conn, line); err != nil {
				return fail(err)
			}
		case line := <-whenReady(m.wifiReq):
			if err := m.write(conn, line); err != nil {
				return fail(err)
			}
		case <-hello.C:
			if !ready {
				if err := sayHello(); err != nil {
					return fail(err)
				}
			}
		case job := <-m.usbFlash:
			if !ready {
				m.flashFailed(errors.New("屏幕未就绪"))
				continue
			}
			restarting, err := m.flash(sctx, conn, msgs, readErr, m.usbKick, job)
			switch {
			case errors.Is(err, errKicked), ctx.Err() != nil:
				return 0
			case err != nil:
				return fail(err)
			case restarting:
				// Say hello until the new firmware answers.
				ready = false
			}
		}
	}
}

// ConfigureWiFi tells the screen, on the USB cable, which WiFi to join.
func (m *Manager) ConfigureWiFi(w WiFiConfig) error {
	if err := w.Check(); err != nil {
		return err
	}
	if err := m.wifiLinked(); err != nil {
		return err
	}
	t := time.NewTimer(2 * time.Second)
	defer t.Stop()
	select {
	case m.wifiReq <- EncodeWiFi(w):
		return nil
	case <-t.C:
		return errors.New("屏幕没有响应，请稍后再试")
	}
}

// ScanWiFi asks the screen, on the USB cable, which networks it sees.
func (m *Manager) ScanWiFi(ctx context.Context) ([]AP, error) {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	if err := m.wifiLinked(); err != nil {
		return nil, err
	}
	select {
	case <-m.scanRes: // the late answer to an earlier scan
	default:
	}
	t := time.NewTimer(m.scanTimeout)
	defer t.Stop()
	select {
	case m.scanReq <- EncodeWiFiScan():
	case <-t.C:
		return nil, errors.New("屏幕没有响应，请稍后再试")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case r := <-m.scanRes:
		if r.err != "" {
			return nil, errors.New(r.err)
		}
		if r.aps == nil {
			r.aps = []AP{}
		}
		return r.aps, nil
	case <-t.C:
		return nil, errors.New("屏幕没有回复扫描结果，请稍后再试")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// wifiLinked says why the screen's WiFi cannot be set up now, if it cannot.
func (m *Manager) wifiLinked() error {
	switch st := m.Status(); {
	case st.USB.State != StateReady:
		return errors.New("请用 USB 线连接屏幕：WiFi 只能通过 USB 设置")
	case st.Flash.Busy():
		return errors.New("固件正在升级")
	}
	return nil
}

// sendSnapshot sends the current state and returns the hub snapshot it
// came from.
func (m *Manager) sendSnapshot(conn io.WriteCloser) (model.Snapshot, error) {
	m.mu.Lock()
	seq := m.seq + 1
	opts := m.opts
	m.mu.Unlock()

	snap := m.src.Snapshot()
	line, err := EncodeSnapshot(BuildFrame(snap, opts, seq))
	if err != nil {
		return snap, err
	}
	if err := m.write(conn, line); err != nil {
		return snap, err
	}
	m.mu.Lock()
	m.seq = seq
	m.status.Sent++
	m.status.LastSent = time.Now()
	m.mu.Unlock()
	return snap, nil
}

// alert beeps when a task started waiting for the user since the last
// snapshot the screen got, else when one finished.
func (m *Manager) alert(conn io.WriteCloser, s model.Snapshot) error {
	now := waitingTasks(s)
	fresh := false
	for k := range now {
		fresh = fresh || !m.waiting[k]
	}
	m.waiting = now
	var finished bool
	m.active, finished = activeTasks(s, m.active)
	opts := m.options()
	switch {
	case fresh && len(opts.BeepNotes) > 0:
		m.log.Debug("有任务等待处理，设备响铃", "notes", len(opts.BeepNotes))
		return m.write(conn, EncodeBeep(opts.BeepNotes, opts.Volume))
	case finished && len(opts.DoneNotes) > 0:
		m.log.Debug("有任务完成，设备响铃", "notes", len(opts.DoneNotes))
		return m.write(conn, EncodeBeep(opts.DoneNotes, opts.Volume))
	}
	return nil
}

// Beep plays notes on the device now at vol percent, to try a sound out.
func (m *Manager) Beep(notes []Note, vol int) error {
	switch st := m.Status(); {
	case st.State != StateReady:
		return errors.New("设备未连接")
	}
	select {
	case m.beepReq <- EncodeBeep(notes, vol):
	default: // one is on its way already
	}
	return nil
}

// readLoop parses device lines. Lines longer than MaxLine are skipped;
// lines that are not protocol messages (boot logs) are logged at debug.
func (m *Manager) readLoop(ctx context.Context, r io.Reader, out chan<- DeviceMsg, errc chan<- error) {
	br := bufio.NewReaderSize(r, MaxLine)
	skipping := false
	for {
		line, err := br.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			if !skipping {
				m.log.Debug("丢弃超长设备行", "bytes", len(line))
			}
			skipping = true
			continue
		case err != nil:
			if err == io.EOF {
				err = errors.New("设备已断开")
			}
			errc <- err
			return
		}
		if skipping {
			skipping = false // tail of an oversized line
			continue
		}
		msg, derr := DecodeDeviceLine(line)
		if derr != nil {
			m.log.Debug("设备非协议输出", "line", string(line[:min(len(line), 200)]))
			continue
		}
		select {
		case out <- msg:
		case <-ctx.Done():
			return
		}
	}
}
