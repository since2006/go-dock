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
	src := &fakeSource{snap: sampleSnapshot(), ch: make(chan struct{}, 1)}
	ft := &flashTest{logs: &syncBuffer{}}
	ft.m, ft.devices = newTestManager(t, src, ft.logs)
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
	ft.dev.next(t, "snapshot")
	ft.dev.send(t, hello)
	// The firmware version shows before the handshake completes.
	waitFor(t, "device hello", func() bool {
		st := ft.m.Status()
		return st.Firmware != "" && (st.State == StateReady || st.Incompatible)
	})
	return ft
}

const helloOTA = `{"t":"hello","proto":1,"fw":"0.1.0","ota":1,"w":600,"h":450}`

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
	size := int(begin["size"].(float64))
	ft.dev.send(t, `{"t":"ota","state":"ready"}`)
	var got []byte
	for len(got) < size {
		d := ft.dev.next(t, "ota_data")
		if off := int(d["off"].(float64)); off != len(got) {
			t.Fatalf("chunk at %d, have %d bytes", off, len(got))
		}
		b, err := base64.StdEncoding.DecodeString(d["data"].(string))
		if err != nil || len(b) > OTAChunk {
			t.Fatalf("chunk: %d bytes, %v", len(b), err)
		}
		got = append(got, b...)
		ft.dev.send(t, fmt.Sprintf(`{"t":"ota","state":"ack","off":%d}`, len(got)))
	}
	ft.dev.next(t, "ota_end")
	ft.dev.send(t, `{"t":"ota","state":"done"}`)
	waitFor(t, "restarting", func() bool { return ft.m.Status().Flash.State == FlashRestarting })
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
	if st := ft.m.Status(); !st.OTA || st.State != StateReady {
		t.Fatalf("status = %+v", st)
	}
	img := testImage("0.2.0", 2*OTAChunk+904)
	ft.start(t, img)
	if err := ft.m.StartFlash(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "正在升级") {
		t.Fatalf("second update: %v", err)
	}

	if got := ft.receive(t); !bytes.Equal(got, img) {
		t.Fatal("device got a different image")
	}
	if f := ft.m.Status().Flash; f.Version != "0.2.0" || f.Sent != len(img) || f.Total != len(img) {
		t.Fatalf("flash = %+v", f)
	}

	// The new firmware says hello.
	ft.dev.send(t, `{"t":"hello","proto":1,"fw":"0.2.0","ota":1,"w":600,"h":450}`)
	waitFor(t, "done", func() bool { return ft.m.Status().Flash.State == FlashDone })
	ft.dev.next(t, "snapshot")
	if st := ft.m.Status(); st.Firmware != "0.2.0" || st.State != StateReady {
		t.Fatalf("status = %+v", st)
	}
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
	ft.dev.next(t, "snapshot") // back to normal
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
	ft := newFlashTest(t, `{"t":"hello","proto":2,"fw":"9.0.0","ota":1}`, nil)
	waitFor(t, "incompatible", func() bool { return ft.m.Status().Incompatible })
	ft.start(t, testImage("0.2.0", 300))
	ft.receive(t)
}

func TestFlashRefused(t *testing.T) {
	ft := newFlashTest(t, `{"t":"hello","proto":1,"fw":"0.1.0","w":600,"h":450}`, nil)
	if err := ft.m.StartFlash(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "不支持在线升级") {
		t.Fatalf("old firmware: %v", err)
	}

	ft = newFlashTest(t, helloOTA, nil)
	merged := testImage("0.2.0", 300)
	binary.LittleEndian.PutUint32(merged[0x20:], 0)
	ft.start(t, merged)
	ft.failed(t, "不是固件程序镜像")

	ft.m.StartFlash(context.Background(), func(context.Context) ([]byte, error) { return nil, errors.New("下载失败: 404") })
	ft.failed(t, "下载失败")
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
