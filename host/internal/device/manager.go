package device

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/since2006/go-dock/host/internal/logx"
	"github.com/since2006/go-dock/host/internal/model"
)

// Options configures the device link.
type Options struct {
	Enabled     bool
	Port        string // "auto" or an explicit port path
	MaxSessions int
	ShowIdle    bool
}

// SnapshotSource is what the manager mirrors to the device; *hub.Hub
// implements it.
type SnapshotSource interface {
	Snapshot() model.Snapshot
	Subscribe() (<-chan struct{}, func())
}

// Link states reported in Status.State.
const (
	StateDisabled  = "disabled"
	StateSearching = "searching"
	StateConnected = "connected" // port open, no device hello yet
	StateReady     = "ready"     // device answered hello
)

// Status is the link state shown on the web UI.
type Status struct {
	State    string `json:"state"`
	Port     string `json:"port,omitempty"`
	Firmware string `json:"firmware,omitempty"`
	// OTA: the firmware can be updated over the link.
	OTA bool `json:"ota,omitempty"`
	// Incompatible: the device speaks another protocol version; it gets no
	// snapshots but can still be updated.
	Incompatible bool        `json:"incompatible,omitempty"`
	Width        int         `json:"width,omitempty"`
	Height       int         `json:"height,omitempty"`
	LastSent     time.Time   `json:"last_sent,omitzero"`
	LastHeard    time.Time   `json:"last_heard,omitzero"`
	Error        string      `json:"error,omitempty"`
	Sent         uint64      `json:"sent"`
	Flash        FlashStatus `json:"flash,omitzero"` // kept across reconnects
}

// Manager finds the device, keeps the serial link up and pushes snapshots.
type Manager struct {
	src         SnapshotSource
	hostVersion string
	log         *slog.Logger
	onChange    func()

	// Replaceable in tests.
	list         func() ([]PortInfo, error)
	open         func(name string) (io.ReadWriteCloser, error)
	heartbeat    time.Duration
	helloEvery   time.Duration
	rescan       time.Duration
	debounce     time.Duration
	writeTimeout time.Duration
	// Firmware update: waits for the device to erase (otaReady), write a
	// chunk (otaAck) and check the image (otaDone); for the serve loop to
	// take the image (otaPickup); and for the new firmware to say hello
	// (otaReboot), ignoring the old one for otaSettle after it accepted.
	otaReady, otaAck, otaDone, otaPickup, otaReboot, otaSettle time.Duration

	kick     chan struct{}
	flashReq chan flashJob

	mu          sync.Mutex
	opts        Options
	status      Status
	seq         uint64
	flashDoneAt time.Time
}

// NewManager creates the link manager. onChange, if set, is called when a
// firmware update makes progress.
func NewManager(src SnapshotSource, opts Options, hostVersion string, log *slog.Logger, onChange func()) *Manager {
	return &Manager{
		src:          src,
		hostVersion:  hostVersion,
		log:          log.With("component", "device"),
		onChange:     onChange,
		list:         ListPorts,
		open:         openSerial,
		heartbeat:    5 * time.Second,
		helloEvery:   5 * time.Second,
		rescan:       2 * time.Second,
		debounce:     200 * time.Millisecond,
		writeTimeout: 10 * time.Second,
		otaReady:     60 * time.Second,
		otaAck:       10 * time.Second,
		otaDone:      30 * time.Second,
		otaPickup:    5 * time.Second,
		otaReboot:    60 * time.Second,
		otaSettle:    5 * time.Second,
		kick:         make(chan struct{}, 1),
		flashReq:     make(chan flashJob),
		opts:         opts,
		status:       Status{State: StateSearching},
	}
}

// SetOptions applies new options, reconnecting when the port selection or
// enablement changed.
func (m *Manager) SetOptions(o Options) {
	m.mu.Lock()
	old := m.opts
	m.opts = o
	m.mu.Unlock()
	if old.Enabled != o.Enabled || old.Port != o.Port {
		m.Reconnect()
	}
}

// Reconnect drops the current link and searches again immediately.
func (m *Manager) Reconnect() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
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

// setError records err, logging it only when it changes to avoid a line
// every rescan.
func (m *Manager) setError(err error) {
	msg := err.Error()
	m.mu.Lock()
	changed := m.status.Error != msg
	m.status.Error = msg
	m.mu.Unlock()
	if changed {
		m.log.Warn("设备连接问题", "err", msg)
	}
}

// Run keeps the link alive until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	for ctx.Err() == nil {
		opts := m.options()
		if !opts.Enabled {
			m.update(func(s *Status) { *s = Status{State: StateDisabled, Sent: s.Sent, Flash: s.Flash} })
			m.wait(ctx, 0)
			continue
		}
		m.update(unlinked)
		name, err := m.pick(opts.Port)
		if err != nil {
			m.setError(err)
			m.wait(ctx, m.rescan)
			continue
		}
		conn, err := m.open(name)
		if err != nil {
			m.setError(fmt.Errorf("打开 %s: %w", name, err))
			m.wait(ctx, m.rescan)
			continue
		}
		kicked := m.serve(ctx, name, conn)
		conn.Close()
		m.update(unlinked)
		if !kicked {
			m.wait(ctx, m.rescan)
		}
	}
}

// unlinked forgets the device while searching for one.
func unlinked(s *Status) {
	s.State, s.Port, s.Firmware, s.OTA, s.Incompatible, s.Width, s.Height = StateSearching, "", "", false, false, 0, 0
}

// wait sleeps for d (forever when d is 0) or until a reconnect request.
func (m *Manager) wait(ctx context.Context, d time.Duration) {
	var timer <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-ctx.Done():
	case <-m.kick:
	case <-timer:
	}
}

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
		return "", errors.New("未找到设备（VID 303A）")
	}
	return name, nil
}

// serve runs one connection until it fails, ctx ends or a reconnect is
// requested (then it returns true).
func (m *Manager) serve(ctx context.Context, name string, conn io.ReadWriteCloser) (kicked bool) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	m.update(func(s *Status) { s.State, s.Port, s.Error = StateConnected, name, "" })
	m.log.Info("设备串口已打开", "port", name)
	defer m.log.Info("设备串口已关闭", "port", name)

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

	ready, incompatible := false, false
	fail := func(err error) bool {
		m.setError(fmt.Errorf("%s: %w", name, err))
		return false
	}
	if err := m.write(conn, EncodeHello(m.hostVersion)); err != nil {
		return fail(err)
	}
	if err := m.sendSnapshot(conn); err != nil {
		return fail(err)
	}
	for {
		select {
		case <-ctx.Done():
			return false
		case <-m.kick:
			return true
		case err := <-readErr:
			return fail(err)
		case msg := <-msgs:
			m.update(func(s *Status) { s.LastHeard = time.Now() })
			switch msg.T {
			case "hello":
				if m.flashed(msg.FW) {
					continue // the old firmware, about to restart
				}
				// Firmware and update support count even when the protocol
				// does not match: that is what an update fixes.
				m.update(func(s *Status) { s.Firmware, s.OTA = msg.FW, msg.OTA > 0 })
				if msg.Proto != ProtoVersion {
					incompatible = true
					m.update(func(s *Status) { s.Incompatible = true })
					m.setError(fmt.Errorf("设备协议版本 %d 与主机 %d 不兼容，已停止发送", msg.Proto, ProtoVersion))
					continue
				}
				incompatible = false
				if !ready {
					m.log.Info("设备握手完成", "fw", msg.FW, "w", msg.W, "h", msg.H)
				}
				ready = true
				m.update(func(s *Status) {
					s.State, s.Incompatible, s.Width, s.Height, s.Error = StateReady, false, msg.W, msg.H, ""
				})
				if err := m.sendSnapshot(conn); err != nil {
					return fail(err)
				}
			case "ack":
			case "log":
				m.log.Log(sctx, logx.ParseLevel(msg.Level), msg.Msg, "src", "device")
			default:
				m.log.Debug("忽略设备消息", "t", msg.T)
			}
		case <-changes:
			if debounce == nil {
				debounce = time.After(m.debounce)
			}
		case <-debounce:
			debounce = nil
			if !incompatible {
				if err := m.sendSnapshot(conn); err != nil {
					return fail(err)
				}
			}
		case <-heartbeat.C:
			if !incompatible {
				if err := m.sendSnapshot(conn); err != nil {
					return fail(err)
				}
			}
		case <-hello.C:
			if !ready && !incompatible {
				if err := m.write(conn, EncodeHello(m.hostVersion)); err != nil {
					return fail(err)
				}
			}
		case job := <-m.flashReq:
			if !(ready || incompatible) || !m.Status().OTA {
				m.flashFailed(errors.New("设备未就绪"))
				continue
			}
			restarting, err := m.flash(sctx, conn, msgs, readErr, job)
			switch {
			case errors.Is(err, errKicked):
				return true
			case ctx.Err() != nil:
				return false
			case err != nil:
				return fail(err)
			case restarting:
				// Say hello until the new firmware answers.
				ready = false
			case !incompatible:
				if err := m.sendSnapshot(conn); err != nil {
					return fail(err)
				}
			}
		}
	}
}

func (m *Manager) sendSnapshot(conn io.WriteCloser) error {
	m.mu.Lock()
	seq := m.seq + 1
	opts := m.opts
	m.mu.Unlock()

	line, err := EncodeSnapshot(BuildFrame(m.src.Snapshot(), opts, seq))
	if err != nil {
		return err
	}
	if err := m.write(conn, line); err != nil {
		return err
	}
	m.mu.Lock()
	m.seq = seq
	m.status.Sent++
	m.status.LastSent = time.Now()
	m.mu.Unlock()
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
