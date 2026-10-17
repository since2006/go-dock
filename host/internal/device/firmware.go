package device

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"syscall"
	"time"

	"github.com/since2006/go-dock/host/internal/logx"
)

// Firmware update over the link (docs/protocol-v1.md §3.4): the host sends
// the app image in OTAChunk pieces, one at a time, and the device writes
// them to its spare app partition, then restarts into it.

// Image header fields of an ESP-IDF app image: esp_image_header_t, then
// the first segment header, then esp_app_desc_t.
const (
	imageMagic   = 0xE9
	chipESP32S3  = 9
	appDescMagic = 0xABCD5432
	appVersion   = 0x30
	appProject   = 0x50
	appHeaderLen = 0x70

	// ImageProject is the project name the firmware is built under.
	ImageProject = "go-dock-firmware"
	// MaxImage is the size of the device's app partitions.
	MaxImage = 0x7F0000
)

// CheckImage checks that b is a go-dock app image for the ESP32-S3 and
// returns the version it was built as.
func CheckImage(b []byte) (version string, err error) {
	switch {
	case len(b) < appHeaderLen || b[0] != imageMagic || binary.LittleEndian.Uint32(b[0x20:]) != appDescMagic:
		return "", errors.New("不是固件程序镜像（合并镜像不能用于在线升级）")
	case binary.LittleEndian.Uint16(b[12:]) != chipESP32S3:
		return "", errors.New("固件不是给 ESP32-S3 编译的")
	case cString(b[appProject:appProject+32]) != ImageProject:
		return "", fmt.Errorf("固件项目名是 %q，不是 %s", cString(b[appProject:appProject+32]), ImageProject)
	case len(b) > MaxImage:
		return "", fmt.Errorf("固件有 %d 字节，超过分区大小 %d", len(b), MaxImage)
	}
	return cString(b[appVersion : appVersion+32]), nil
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// Firmware update states reported in FlashStatus.State.
const (
	FlashDownloading = "downloading"
	FlashPreparing   = "preparing" // device erasing its spare partition
	FlashSending     = "sending"
	FlashRestarting  = "restarting" // image accepted, waiting for the new firmware
	FlashDone        = "done"
	FlashError       = "error"
)

// FlashStatus is the progress of the last firmware update.
type FlashStatus struct {
	State   string `json:"state"`
	Version string `json:"version,omitempty"`
	Sent    int    `json:"sent,omitempty"`
	Total   int    `json:"total,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Busy reports whether an update is under way.
func (f FlashStatus) Busy() bool {
	switch f.State {
	case FlashDownloading, FlashPreparing, FlashSending, FlashRestarting:
		return true
	}
	return false
}

type flashJob struct {
	image   []byte
	version string
}

// StartFlash updates the device firmware in the background. fetch returns
// the app image (downloaded and verified); it is checked, then streamed to
// the device. Progress shows up in Status().Flash.
func (m *Manager) StartFlash(ctx context.Context, fetch func(context.Context) ([]byte, error)) error {
	m.mu.Lock()
	s := m.status
	var err error
	switch {
	case s.Flash.Busy():
		err = errors.New("固件正在升级")
	case s.State != StateReady && !s.Incompatible:
		err = errors.New("设备未连接")
	case !s.OTA:
		err = errors.New("设备当前的固件不支持在线升级，请先用 USB 刷一次")
	}
	if err == nil {
		m.status.Flash = FlashStatus{State: FlashDownloading}
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	m.changed()

	go func() {
		img, err := fetch(ctx)
		var version string
		if err == nil {
			version, err = CheckImage(img)
		}
		if err != nil {
			m.flashFailed(err)
			return
		}
		m.setFlash(func(f *FlashStatus) { f.Version, f.Total = version, len(img) })
		t := time.NewTimer(m.otaPickup)
		defer t.Stop()
		select {
		case m.flashReq <- flashJob{image: img, version: version}:
		case <-t.C:
			m.flashFailed(errors.New("设备已断开"))
		case <-ctx.Done():
			m.flashFailed(ctx.Err())
		}
	}()
	return nil
}

func (m *Manager) setFlash(f func(*FlashStatus)) {
	m.mu.Lock()
	f(&m.status.Flash)
	m.mu.Unlock()
	m.changed()
}

func (m *Manager) flashFailed(err error) {
	m.log.Warn("固件升级失败", "err", err)
	m.setFlash(func(f *FlashStatus) { f.State, f.Error = FlashError, err.Error() })
}

func (m *Manager) changed() {
	if m.onChange != nil {
		m.onChange()
	}
}

var (
	errKicked       = errors.New("重新连接")
	errOTATimeout   = errors.New("设备没有响应")
	errOTARestarted = errors.New("设备中途重启")
)

// otaError is the device refusing or failing the update; the link is fine.
type otaError string

func (e otaError) Error() string { return string(e) }

// flash streams job to the device, which then restarts into the new
// firmware. It runs on the serve goroutine, so nothing else is sent
// meanwhile. A failed update leaves the link up and is reported in
// Status().Flash; the returned error is for the link: it failed, ctx ended
// or a reconnect was requested (errKicked).
func (m *Manager) flash(ctx context.Context, conn io.WriteCloser, msgs <-chan DeviceMsg, readErr <-chan error, job flashJob) (restarting bool, err error) {
	total := len(job.image)
	m.log.Info("开始升级设备固件", "ver", job.version, "bytes", total)
	m.setFlash(func(f *FlashStatus) { f.State, f.Sent = FlashPreparing, 0 })

	// next waits for the device's next ota message, forwarding its logs.
	next := func(d time.Duration) (DeviceMsg, error) {
		t := time.NewTimer(d)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return DeviceMsg{}, ctx.Err()
			case <-m.kick:
				return DeviceMsg{}, errKicked
			case err := <-readErr:
				return DeviceMsg{}, err
			case <-t.C:
				return DeviceMsg{}, errOTATimeout
			case msg := <-msgs:
				m.update(func(s *Status) { s.LastHeard = time.Now() })
				switch msg.T {
				case "ota":
					if msg.State == "error" {
						return msg, otaError(msg.Msg)
					}
					return msg, nil
				case "hello":
					return msg, errOTARestarted
				case "log":
					m.log.Log(ctx, logx.ParseLevel(msg.Level), msg.Msg, "src", "device")
				}
			}
		}
	}
	// step sends line and waits for the answer in state want.
	step := func(line []byte, d time.Duration, want string) (DeviceMsg, error) {
		if err := m.write(conn, line); err != nil {
			return DeviceMsg{}, err
		}
		msg, err := next(d)
		if err == nil && msg.State != want {
			err = otaError(fmt.Sprintf("设备回复 %q，应为 %q", msg.State, want))
		}
		return msg, err
	}

	err = func() error {
		if _, err := step(EncodeOTABegin(total, job.version), m.otaReady, "ready"); err != nil {
			return err
		}
		m.setFlash(func(f *FlashStatus) { f.State = FlashSending })
		pct := 0
		for off := 0; off < total; off += OTAChunk {
			end := min(off+OTAChunk, total)
			msg, err := step(EncodeOTAData(off, job.image[off:end]), m.otaAck, "ack")
			if err != nil {
				return err
			}
			if msg.Off != end {
				return otaError(fmt.Sprintf("设备确认到 %d 字节，应为 %d", msg.Off, end))
			}
			m.mu.Lock()
			m.status.Flash.Sent = end
			m.mu.Unlock()
			if p := end * 100 / total; p != pct {
				pct = p
				m.changed()
			}
		}
		_, err := step(EncodeOTA("ota_end"), m.otaDone, "done")
		return err
	}()

	var oe otaError
	switch {
	case err == nil:
		m.log.Info("固件已写入，设备正在重启", "ver", job.version)
		m.mu.Lock()
		m.status.Flash.State = FlashRestarting
		m.flashDoneAt = time.Now()
		doneAt := m.flashDoneAt
		m.mu.Unlock()
		m.changed()
		time.AfterFunc(m.otaReboot, func() {
			m.mu.Lock()
			late := m.status.Flash.State == FlashRestarting && m.flashDoneAt.Equal(doneAt)
			m.mu.Unlock()
			if late {
				m.flashFailed(fmt.Errorf("设备重启后没有报告新版本 %s，请拔插 USB 后查看", job.version))
			}
		})
		return true, nil
	case errors.As(err, &oe), errors.Is(err, errOTATimeout), errors.Is(err, errOTARestarted):
		_ = m.write(conn, EncodeOTA("ota_abort"))
		m.flashFailed(err)
		return false, nil
	default:
		if errors.Is(err, errKicked) || ctx.Err() != nil { // the port is still open
			_ = m.write(conn, EncodeOTA("ota_abort"))
		}
		m.flashFailed(fmt.Errorf("连接中断: %w", err))
		return false, err
	}
}

// flashed settles an update from the version a device announces in its
// hello: the new one means success; another one means the new firmware
// did not stay, unless it comes so soon after the image was accepted that
// it is the old firmware answering before its restart. Then it returns
// stale and the hello is to be ignored.
func (m *Manager) flashed(fw string) (stale bool) {
	m.mu.Lock()
	f := &m.status.Flash
	var failed error
	switch {
	case f.State != FlashRestarting:
		m.mu.Unlock()
		return false
	case fw == f.Version:
		f.State = FlashDone
	case time.Since(m.flashDoneAt) > m.otaSettle:
		failed = fmt.Errorf("设备重启后仍是 %s，新固件没能正常启动，已自动回到旧版本", fw)
	default:
		m.mu.Unlock()
		return true
	}
	version := f.Version
	m.mu.Unlock()
	if failed != nil {
		m.flashFailed(failed)
		return false
	}
	m.log.Info("设备固件已升级", "ver", version)
	m.changed()
	return false
}

// write sends b, giving up after writeTimeout: a device that stopped
// reading would otherwise block the link for good. On timeout it closes the
// port so Run reconnects.
func (m *Manager) write(conn io.WriteCloser, b []byte) error {
	done := make(chan error, 1)
	go func() { done <- writeAll(conn, b) }()
	t := time.NewTimer(m.writeTimeout)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
		conn.Close()
		return errors.New("写入超时")
	}
}

// writeAll writes all of b. A serial port's Write is a single write(2),
// which a signal (the Go runtime preempts goroutines with them) can cut
// short or interrupt; the rest of the line would be lost.
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		b = b[max(n, 0):]
		switch {
		case errors.Is(err, syscall.EINTR):
		case err != nil:
			return err
		case n == 0:
			return io.ErrShortWrite
		}
	}
	return nil
}
