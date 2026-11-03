package device

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// testImage builds an app image of size bytes whose header says version.
func testImage(version string, size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i * 7)
	}
	b[0] = imageMagic
	binary.LittleEndian.PutUint16(b[12:], chipESP32S3)
	binary.LittleEndian.PutUint32(b[0x20:], appDescMagic)
	copy(b[appVersion:appVersion+32], make([]byte, 32))
	copy(b[appVersion:], version)
	copy(b[appProject:appProject+32], make([]byte, 32))
	copy(b[appProject:], ImageProject)
	return b
}

func TestCheckImage(t *testing.T) {
	real, err := os.ReadFile("testdata/app-header.bin") // head of a real build
	if err != nil {
		t.Fatal(err)
	}
	if v, err := CheckImage(real); v != "0.1.0" || err != nil {
		t.Errorf("real header: %q, %v", v, err)
	}
	if v, err := CheckImage(testImage("0.2.0-beta.1", 4096)); v != "0.2.0-beta.1" || err != nil {
		t.Errorf("test image: %q, %v", v, err)
	}

	bad := map[string]func(b []byte) []byte{
		"不是固件程序镜像": func(b []byte) []byte { b[0x20] = 0; return b }, // e.g. the merged image
		"ESP32-S3": func(b []byte) []byte { b[12] = 2; return b },
		"项目名":      func(b []byte) []byte { copy(b[appProject:], "other\x00"); return b },
		"超过分区大小":   func(b []byte) []byte { return append(b, make([]byte, MaxImage)...) },
	}
	for want, f := range bad {
		if _, err := CheckImage(f(testImage("0.2.0", 4096))); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
	if _, err := CheckImage([]byte{imageMagic}); err == nil {
		t.Error("short image accepted")
	}
}

func TestOTALinesFit(t *testing.T) {
	if n := len(EncodeOTAData(MaxImage, make([]byte, OTAChunk))); n > MaxLine {
		t.Fatalf("ota_data line is %d bytes", n)
	}
}

// flashTest is a manager linked to a fake device that announced firmware
// fw and update support.
type flashTest struct {
	m       *Manager
	dev     *fakeDevice
	devices chan *fakeDevice
	logs    *syncBuffer
	changes atomic.Int32
	cancel  context.CancelFunc
}

func newFlashTest(t *testing.T, hello string, tune func(m *Manager)) *flashTest {
	t.Helper()
	ft := &flashTest{logs: &syncBuffer{}}
	ft.m, ft.devices = newUSBTestManager(t, ft.logs)
	ft.m.onChange = func() { ft.changes.Add(1) }
	ft.m.otaPickup = time.Second
	if tune != nil {
		tune(ft.m)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ft.cancel = cancel
	t.Cleanup(cancel)
	go ft.m.Run(ctx)
	ft.dev = <-ft.devices
	ft.dev.next(t, "hello")
	ft.dev.send(t, hello)
	waitFor(t, "device hello", func() bool {
		u := ft.m.Status().USB
		return u.Firmware != "" && u.State == StateReady
	})
	return ft
}

const helloOTA = `{"t":"hello","proto":1,"fw":"0.1.0","w":600,"h":450}`

func (ft *flashTest) start(t *testing.T, img []byte) {
	t.Helper()
	err := ft.m.StartFlash(context.Background(), func(context.Context) ([]byte, error) { return img, nil })
	if err != nil {
		t.Fatal(err)
	}
}

// receive plays the device's side of an update and returns the image it
// got.
func (ft *flashTest) receive(t *testing.T) []byte {
	t.Helper()
	begin := ft.dev.next(t, "ota_begin")
	ft.dev.send(t, `{"t":"ota","state":"ready"}`)
	got := takeImage(t, ft.dev, int(begin["size"].(float64)))
	waitFor(t, "restarting", func() bool { return ft.m.Status().Flash.State == FlashRestarting })
	return got
}

// takeImage plays the device's side of an update once it said ready: it
// acks each chunk, then accepts the image.
func takeImage(t *testing.T, dev *fakeDevice, size int) []byte {
	t.Helper()
	var got []byte
	for len(got) < size {
		d := dev.next(t, "ota_data")
		if off := int(d["off"].(float64)); off != len(got) {
			t.Fatalf("chunk at %d, have %d bytes", off, len(got))
		}
		b, err := base64.StdEncoding.DecodeString(d["data"].(string))
		if err != nil || len(b) > OTAChunk {
			t.Fatalf("chunk: %d bytes, %v", len(b), err)
		}
		got = append(got, b...)
		dev.send(t, fmt.Sprintf(`{"t":"ota","state":"ack","off":%d}`, len(got)))
	}
	dev.next(t, "ota_end")
	dev.send(t, `{"t":"ota","state":"done"}`)
	return got
}

func (ft *flashTest) failed(t *testing.T, want string) {
	t.Helper()
	waitFor(t, "update error", func() bool { return ft.m.Status().Flash.State == FlashError })
	if f := ft.m.Status().Flash; !strings.Contains(f.Error, want) {
		t.Fatalf("error = %q, want %q", f.Error, want)
	}
}

func TestFlash(t *testing.T) {
	ft := newFlashTest(t, helloOTA, nil)
	img := testImage("0.2.0", 2*OTAChunk+904)
	ft.start(t, img)
	if f := ft.m.Status().Flash; f.Via != FlashUSB {
		t.Fatalf("flash = %+v", f)
	}
	if err := ft.m.StartFlash(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "正在升级") {
		t.Fatalf("second update: %v", err)
	}

	if got := ft.receive(t); !bytes.Equal(got, img) {
		t.Fatal("device got a different image")
	}
	if f := ft.m.Status().Flash; f.Version != "0.2.0" || f.Sent != len(img) || f.Total != len(img) {
		t.Fatalf("flash = %+v", f)
	}

	// The new firmware says hello, and hears go-dock's: that keeps it.
	ft.dev.send(t, `{"t":"hello","proto":1,"fw":"0.2.0","w":600,"h":450}`)
	waitFor(t, "done", func() bool { return ft.m.Status().Flash.State == FlashDone })
	ft.dev.next(t, "hello")
	waitFor(t, "new version", func() bool { u := ft.m.Status().USB; return u.Firmware == "0.2.0" && u.State == StateReady })
	if ft.changes.Load() < 4 {
		t.Errorf("onChange called %d times", ft.changes.Load())
	}
}

func TestFlashRolledBack(t *testing.T) {
	ft := newFlashTest(t, helloOTA, func(m *Manager) { m.otaSettle = 50 * time.Millisecond })
	ft.start(t, testImage("0.2.0", 300))
	ft.receive(t)

	// Right after the image was accepted, an old hello is the old firmware
	// still answering: ignored.
	ft.dev.send(t, helloOTA)
	time.Sleep(20 * time.Millisecond)
	if f := ft.m.Status().Flash; f.State != FlashRestarting {
		t.Fatalf("early old hello settled the update: %+v", f)
	}
	time.Sleep(50 * time.Millisecond)
	ft.dev.send(t, helloOTA)
	ft.failed(t, "仍是 0.1.0")
}

func TestFlashNoRestart(t *testing.T) {
	ft := newFlashTest(t, helloOTA, func(m *Manager) { m.otaReboot = 50 * time.Millisecond })
	ft.start(t, testImage("0.2.0", 300))
	ft.receive(t)
	ft.failed(t, "没有报告新版本")
}

func TestFlashDeviceError(t *testing.T) {
	ft := newFlashTest(t, helloOTA, nil)
	ft.start(t, testImage("0.2.0", 300))
	ft.dev.next(t, "ota_begin")
	ft.dev.send(t, `{"t":"ota","state":"error","msg":"没有可用的升级分区"}`)
	ft.dev.next(t, "ota_abort")
	ft.failed(t, "没有可用的升级分区")
	// The cable stays up for another try.
	if u := ft.m.Status().USB; u.State != StateReady {
		t.Fatalf("usb = %+v", u)
	}
}

func TestFlashAckTimeout(t *testing.T) {
	ft := newFlashTest(t, helloOTA, func(m *Manager) { m.otaAck = 50 * time.Millisecond })
	ft.start(t, testImage("0.2.0", 3*OTAChunk))
	ft.dev.next(t, "ota_begin")
	ft.dev.send(t, `{"t":"ota","state":"ready"}`)
	ft.dev.next(t, "ota_data")
	ft.dev.next(t, "ota_abort")
	ft.failed(t, "没有响应")
}

func TestFlashUnplugged(t *testing.T) {
	ft := newFlashTest(t, helloOTA, nil)
	ft.start(t, testImage("0.2.0", 3*OTAChunk))
	ft.dev.next(t, "ota_begin")
	ft.dev.send(t, `{"t":"ota","state":"ready"}`)
	ft.dev.next(t, "ota_data")
	ft.dev.conn.Close()
	ft.failed(t, "连接中断")
	(<-ft.devices).next(t, "hello") // reconnects as usual
}

func TestFlashIncompatibleDevice(t *testing.T) {
	ft := newFlashTest(t, `{"t":"hello","proto":2,"fw":"9.0.0"}`, nil)
	waitFor(t, "incompatible", func() bool { return ft.m.Status().USB.Incompatible })
	ft.start(t, testImage("0.2.0", 300))
	ft.receive(t)
}

func TestFlashRefused(t *testing.T) {
	ft := newFlashTest(t, helloOTA, nil)
	merged := testImage("0.2.0", 300)
	binary.LittleEndian.PutUint32(merged[0x20:], 0)
	ft.start(t, merged)
	ft.failed(t, "不是固件程序镜像")

	ft.m.StartFlash(context.Background(), func(context.Context) ([]byte, error) { return nil, errors.New("下载失败: 404") })
	ft.failed(t, "下载失败")
}

// wifiFlash is a manager linked over WiFi to a fake screen running
// firmware 0.1.0.
func wifiFlash(t *testing.T, tune func(m *Manager)) (*Manager, *fakeDevice, chan *fakeDevice) {
	t.Helper()
	m, devices, _ := newNetManager(t, io.Discard)
	m.otaPickup = time.Second
	if tune != nil {
		tune(m)
	}
	runManager(t, m)
	dev := <-devices
	dev.next(t, "hello")
	dev.send(t, `{"t":"hello","proto":1,"fw":"0.1.0","w":600,"h":450}`)
	waitFor(t, "ready", func() bool { return m.Status().State == StateReady })
	return m, dev, devices
}

func startFlash(t *testing.T, m *Manager, img []byte) {
	t.Helper()
	if err := m.StartFlash(context.Background(), func(context.Context) ([]byte, error) { return img, nil }); err != nil {
		t.Fatal(err)
	}
}

func flashFailed(t *testing.T, m *Manager, want string) {
	t.Helper()
	waitFor(t, "update error", func() bool { return m.Status().Flash.State == FlashError })
	if f := m.Status().Flash; !strings.Contains(f.Error, want) {
		t.Fatalf("error = %q, want %q", f.Error, want)
	}
}

// Over WiFi the screen asks the user first. Once they said yes it erases
// and takes the image like over USB, restarts, and its new firmware says
// hello on the next link.
func TestFlashWiFi(t *testing.T) {
	m, dev, devices := wifiFlash(t, nil)
	img := testImage("0.2.0", 2*OTAChunk+100)
	startFlash(t, m, img)
	begin := dev.next(t, "ota_begin")
	dev.send(t, `{"t":"ota","state":"confirm"}`)
	waitFor(t, "confirming", func() bool { f := m.Status().Flash; return f.State == FlashConfirming && f.Via == FlashWiFi })
	dev.send(t, `{"t":"ota","state":"confirmed"}`)
	waitFor(t, "preparing", func() bool { return m.Status().Flash.State == FlashPreparing })
	dev.send(t, `{"t":"ota","state":"ready"}`)
	if got := takeImage(t, dev, int(begin["size"].(float64))); !bytes.Equal(got, img) {
		t.Fatal("screen got a different image")
	}
	waitFor(t, "restarting", func() bool { return m.Status().Flash.State == FlashRestarting })

	dev.conn.Close() // the screen restarts
	dev = <-devices
	dev.next(t, "hello")
	dev.send(t, `{"t":"hello","proto":1,"fw":"0.2.0","w":600,"h":450}`)
	waitFor(t, "done", func() bool { return m.Status().Flash.State == FlashDone })
	if s := m.Status(); s.Firmware != "0.2.0" || s.State != StateReady {
		t.Fatalf("status %+v", s)
	}
}

// Cancelled on the screen, the update fails and the link carries on.
func TestFlashWiFiCancelled(t *testing.T) {
	m, dev, _ := wifiFlash(t, nil)
	startFlash(t, m, testImage("0.2.0", 300))
	dev.next(t, "ota_begin")
	dev.send(t, `{"t":"ota","state":"confirm"}`)
	dev.send(t, `{"t":"ota","state":"error","msg":"已在屏幕上取消"}`)
	dev.next(t, "ota_abort")
	flashFailed(t, m, "已在屏幕上取消")
	dev.next(t, "snapshot") // the screen gets the state again
	if s := m.Status(); s.State != StateReady {
		t.Fatalf("status %+v", s)
	}
}

// A screen that never answers the question is given up on.
func TestFlashWiFiUnanswered(t *testing.T) {
	m, dev, _ := wifiFlash(t, func(m *Manager) { m.otaConfirm = 50 * time.Millisecond })
	startFlash(t, m, testImage("0.2.0", 300))
	dev.next(t, "ota_begin")
	dev.send(t, `{"t":"ota","state":"confirm"}`)
	dev.next(t, "ota_abort")
	flashFailed(t, m, "没有响应")
}

// A screen speaking another protocol gets no data over WiFi, but can be
// updated there: that is what fixes it.
func TestFlashWiFiIncompatible(t *testing.T) {
	m, devices, _ := newNetManager(t, io.Discard)
	m.otaPickup = time.Second
	runManager(t, m)
	dev := <-devices
	dev.send(t, `{"t":"hello","proto":2,"fw":"9.0.0"}`)
	waitFor(t, "incompatible", func() bool { return m.Status().Incompatible })
	startFlash(t, m, testImage("0.2.0", 300))
	dev.next(t, "ota_begin")
	dev.send(t, `{"t":"ota","state":"confirm"}`)
	waitFor(t, "confirming", func() bool { return m.Status().Flash.State == FlashConfirming })
}

func TestWriteTimeout(t *testing.T) {
	m, _ := newTestManager(t, &fakeSource{ch: make(chan struct{}, 1)}, io.Discard)
	m.writeTimeout = 20 * time.Millisecond
	host, dev := net.Pipe() // nobody reads dev
	defer dev.Close()
	if err := m.write(host, []byte("{}\n")); err == nil || !strings.Contains(err.Error(), "写入超时") {
		t.Fatalf("write = %v", err)
	}
	if _, err := host.Write([]byte("x")); err == nil {
		t.Fatal("port left open after a write timeout")
	}
}

// choppyWriter writes at most 3 bytes per call and is interrupted every
// other call, like a serial port's write(2) hit by signals.
type choppyWriter struct {
	bytes.Buffer
	calls int
}

func (w *choppyWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls%2 == 0 {
		return 0, syscall.EINTR
	}
	return w.Buffer.Write(p[:min(len(p), 3)])
}

func TestWriteAll(t *testing.T) {
	var w choppyWriter
	if err := writeAll(&w, []byte("{\"t\":\"ota_end\"}\n")); err != nil || w.String() != "{\"t\":\"ota_end\"}\n" {
		t.Fatalf("writeAll: %q, %v", w.String(), err)
	}
}
