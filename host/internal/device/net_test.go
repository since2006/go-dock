package device

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestNetAddr(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.50":       "192.168.1.50:47322",
		" 192.168.1.50 ":     "192.168.1.50:47322",
		"192.168.1.50:1234":  "192.168.1.50:1234",
		"dock.lan":           "dock.lan:47322",
		"[fe80::1]:47322":    "[fe80::1]:47322",
		"":                   "",
		"192.168.1.50:":      "",
		"192.168.1.50:0":     "",
		"192.168.1.50:99999": "",
		"a:b:c":              "",
		"http://192.168.1.5": "",
	} {
		got, err := NetAddr(in)
		if want == "" {
			if err == nil {
				t.Errorf("NetAddr(%q) = %q, want an error", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("NetAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestWiFiCheck(t *testing.T) {
	ok := []struct {
		in, want WiFiConfig
	}{
		{WiFiConfig{SSID: "home", Pass: "12345678"}, WiFiConfig{SSID: "home", Pass: "12345678"}},
		{WiFiConfig{SSID: "cafe"}, WiFiConfig{SSID: "cafe"}}, // open network
		// DHCP drops what only a fixed IP uses.
		{WiFiConfig{SSID: "home", Mask: "255.0.0.0", GW: "10.0.0.1"}, WiFiConfig{SSID: "home"}},
		{WiFiConfig{SSID: "home", IP: "192.168.1.50"}, WiFiConfig{SSID: "home", IP: "192.168.1.50", Mask: "255.255.255.0"}},
		{WiFiConfig{SSID: "home", IP: "10.0.0.9", Mask: "255.0.0.0", GW: "10.0.0.1"}, WiFiConfig{SSID: "home", IP: "10.0.0.9", Mask: "255.0.0.0", GW: "10.0.0.1"}},
		// No SSID: WiFi off, nothing else kept.
		{WiFiConfig{Pass: "12345678", IP: "10.0.0.9"}, WiFiConfig{}},
	}
	for _, c := range ok {
		w := c.in
		if err := w.Check(); err != nil || w != c.want {
			t.Errorf("Check(%+v) = %+v, %v; want %+v", c.in, w, err, c.want)
		}
	}
	for _, w := range []WiFiConfig{
		{SSID: strings.Repeat("x", 33)},
		{SSID: "  "},
		{SSID: "home", Pass: "short"},
		{SSID: "home", Pass: strings.Repeat("x", 64)},
		{SSID: "home", IP: "192.168.1"},
		{SSID: "home", IP: "fe80::1"},
		{SSID: "home", IP: "192.168.1.50", Mask: "nope"},
		{SSID: "home", IP: "192.168.1.50", GW: "1.2.3"},
	} {
		if err := w.Check(); err == nil {
			t.Errorf("Check(%+v) passed", w)
		}
	}
}

func TestEncodeWiFi(t *testing.T) {
	line := EncodeWiFi(WiFiConfig{SSID: "家里<5G>", Pass: "p&ss word", IP: "192.168.1.50", Mask: "255.255.255.0"})
	want := `{"t":"wifi","ssid":"家里<5G>","pass":"p&ss word","ip":"192.168.1.50","mask":"255.255.255.0"}` + "\n"
	if string(line) != want {
		t.Fatalf("got  %s\nwant %s", line, want)
	}
	if got := string(EncodeWiFi(WiFiConfig{})); got != `{"t":"wifi","ssid":"","pass":""}`+"\n" {
		t.Fatalf("off: %s", got)
	}
}

func TestEncodeHello(t *testing.T) {
	var h Hello
	if err := json.Unmarshal(EncodeHello("一二三四五六七八九十甲乙丙丁戊己庚辛壬癸子", "0123456789abcdef", "123456"), &h); err != nil {
		t.Fatal(err)
	}
	if h.T != "hello" || h.Proto != ProtoVersion || h.Name != "一二三四五六七八九十甲乙丙丁戊己庚辛壬癸" || h.ID != "0123456789abcdef" || h.Code != "123456" {
		t.Fatalf("hello = %+v", h)
	}
	h = Hello{}
	if err := json.Unmarshal(EncodeHello("mini", "Not An ID", ""), &h); err != nil {
		t.Fatal(err)
	}
	if h.ID != "" || h.Code != "" {
		t.Fatalf("bad id or empty code sent: %+v", h)
	}
}

func TestNetStatusAddr(t *testing.T) {
	for _, c := range []struct {
		n    NetStatus
		want string
	}{
		{NetStatus{State: "connecting"}, ""},
		{NetStatus{IP: "192.168.1.50"}, "192.168.1.50"},
		{NetStatus{IP: "192.168.1.50", Port: NetPort}, "192.168.1.50"},
		{NetStatus{IP: "192.168.1.50", Port: 8080}, "192.168.1.50:8080"},
	} {
		if got := c.n.Addr(); got != c.want {
			t.Errorf("%+v.Addr() = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestDialError(t *testing.T) {
	refused := &net.OpError{Op: "dial", Err: &osSyscallError{syscall.ECONNREFUSED}}
	if err := dialError("1.2.3.4:47322", refused); !strings.Contains(err.Error(), "拒绝连接") {
		t.Fatalf("refused: %v", err)
	}
	if err := dialError("1.2.3.4:47322", timeoutErr{}); !strings.Contains(err.Error(), "IP 不对") {
		t.Fatalf("timeout: %v", err)
	}
	if err := dialError("1.2.3.4:47322", errors.New("boom")); !strings.Contains(err.Error(), "boom") {
		t.Fatalf("other: %v", err)
	}
}

type osSyscallError struct{ err error }

func (e *osSyscallError) Error() string { return e.err.Error() }
func (e *osSyscallError) Unwrap() error { return e.err }

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// newNetManager is newTestManager that also reports each address dialed.
func newNetManager(t *testing.T, logs io.Writer) (*Manager, chan *fakeDevice, chan string) {
	t.Helper()
	src := &fakeSource{snap: sampleSnapshot(), ch: make(chan struct{}, 1)}
	m, devices := newTestManager(t, src, logs)
	dialed := make(chan string, 8)
	dial := m.dial
	m.dial = func(addr string) (io.ReadWriteCloser, error) {
		dialed <- addr
		return dial(addr)
	}
	return m, devices, dialed
}

func runManager(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Run did not return")
		}
	})
}

func TestManagerNet(t *testing.T) {
	m, devices, dialed := newNetManager(t, io.Discard)
	runManager(t, m)

	if addr := <-dialed; addr != "192.168.1.50:47322" {
		t.Fatalf("dialed %s", addr)
	}
	dev := <-devices
	h := dev.next(t, "hello")
	if h["code"] != "123456" || h["name"] != "mini" || h["id"] != "abc123" {
		t.Fatalf("hello = %v", h)
	}
	dev.next(t, "snapshot")
	dev.send(t, `{"t":"hello","proto":1,"fw":"0.2.0","w":600,"h":450}`)
	waitFor(t, "ready", func() bool { return m.Status().State == StateReady })
	if st := m.Status(); st.Addr != "192.168.1.50:47322" || st.Firmware != "0.2.0" {
		t.Fatalf("status = %+v", st)
	}
	// WiFi is set up over the cable only, never over WiFi.
	if _, err := m.ConfigureWiFi(WiFiConfig{SSID: "home", Pass: "12345678"}); err == nil || !strings.Contains(err.Error(), "USB") {
		t.Fatalf("wifi without the cable: %v", err)
	}
	dev.send(t, `{"t":"net","state":"connected","ip":"192.168.1.50","code":"123456"}`)
	dev.send(t, `{"t":"ack","seq":2}`)
	waitFor(t, "ack read", func() bool { return !m.Status().LastHeard.IsZero() })
	if m.Status().USB.Net != nil {
		t.Fatal("net status taken over WiFi")
	}

	// The screen hangs up: go-dock dials again.
	dev.conn.Close()
	<-dialed
	dev = <-devices
	dev.next(t, "hello")
}

func TestManagerNetDenied(t *testing.T) {
	logs := &syncBuffer{}
	m, devices, dialed := newNetManager(t, logs)
	m.deniedWait = 200 * time.Millisecond
	runManager(t, m)

	<-dialed
	dev := <-devices
	dev.next(t, "hello")
	dev.send(t, `{"t":"denied","msg":"配对码不对"}`)
	waitFor(t, "denied", func() bool { return m.Status().Error == "配对码不对" })
	start := time.Now()
	<-dialed
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("dialed again after %v, want the denied wait", d)
	}
	// The reason stays up while the next try is on its way, and is not
	// logged again.
	dev = <-devices
	dev.next(t, "hello")
	if st := m.Status(); st.Error != "配对码不对" {
		t.Fatalf("status = %+v", st)
	}
	dev.send(t, `{"t":"denied","msg":"配对码不对"}`)
	<-dialed
	if n := strings.Count(logs.String(), "配对码不对"); n != 1 {
		t.Fatalf("denial logged %d times:\n%s", n, logs.String())
	}
	// A handshake clears it.
	dev = <-devices
	dev.send(t, `{"t":"hello","proto":1,"fw":"0.2.0"}`)
	waitFor(t, "ready", func() bool { st := m.Status(); return st.State == StateReady && st.Error == "" })
}

func TestManagerNetSilence(t *testing.T) {
	m, devices, dialed := newNetManager(t, io.Discard)
	m.netSilence = 80 * time.Millisecond
	runManager(t, m)

	<-dialed
	dev := <-devices
	dev.send(t, `{"t":"hello","proto":1,"fw":"0.2.0"}`)
	waitFor(t, "ready", func() bool { return m.Status().State == StateReady })
	// Messages keep the link up...
	for range 3 {
		time.Sleep(40 * time.Millisecond)
		dev.send(t, `{"t":"ack","seq":1}`)
	}
	select {
	case <-dialed:
		t.Fatal("dropped a link that was talking")
	default:
	}
	// ...silence ends it.
	select {
	case <-dialed:
	case <-time.After(2 * time.Second):
		t.Fatal("silent link kept")
	}
	if err := m.Status().Error; !strings.Contains(err, "没有收到屏幕的消息") {
		t.Fatalf("error = %q", err)
	}
}

func TestManagerNetDialFails(t *testing.T) {
	m, _, _ := newNetManager(t, io.Discard)
	m.dial = func(addr string) (io.ReadWriteCloser, error) {
		return nil, &net.OpError{Op: "dial", Err: &osSyscallError{syscall.ECONNREFUSED}}
	}
	runManager(t, m)
	waitFor(t, "error", func() bool {
		st := m.Status()
		return st.State == StateSearching && strings.Contains(st.Error, "拒绝连接")
	})
}

func TestManagerCodeChange(t *testing.T) {
	m, devices, dialed := newNetManager(t, io.Discard)
	runManager(t, m)
	<-dialed
	<-devices
	// Changing the code reconnects with the new one.
	o := m.options()
	o.Code = "654321"
	m.SetOptions(o)
	<-dialed
	if h := (<-devices).next(t, "hello"); h["code"] != "654321" {
		t.Fatalf("hello after the change = %v", h)
	}
}

// usbScreen links a manager to a fake screen on the USB cable that said
// hello.
func usbScreen(t *testing.T, onChange func()) (*Manager, *fakeDevice, chan *fakeDevice) {
	t.Helper()
	m, devices := newUSBTestManager(t, io.Discard)
	m.onChange = onChange
	runManager(t, m)
	dev := <-devices
	if h := dev.next(t, "hello"); h["code"] != nil {
		t.Fatalf("pairing code sent over USB: %v", h)
	}
	dev.send(t, `{"t":"hello","proto":1,"fw":"0.2.0"}`)
	waitFor(t, "usb ready", func() bool { return m.Status().USB.State == StateReady })
	return m, dev, devices
}

func TestUSBCarriesNoData(t *testing.T) {
	m, dev, _ := usbScreen(t, nil)
	if st := m.Status(); st.State != StateUnset || st.USB.Port != "/dev/cu.usbmodem1" || st.USB.Firmware != "0.2.0" {
		t.Fatalf("status = %+v", st)
	}
	select {
	case msg := <-dev.lines:
		t.Fatalf("sent %v over USB", msg)
	case <-time.After(100 * time.Millisecond):
	}
}

// A screen that restarts on its own says hello unasked; go-dock answers,
// once, so a firmware on probation hears it.
func TestUSBAnswersUnaskedHello(t *testing.T) {
	m, dev, _ := usbScreen(t, nil)
	dev.none(t, "hello", 50*time.Millisecond) // the answer to ours needs none
	dev.send(t, `{"t":"hello","proto":1,"fw":"0.2.0"}`)
	dev.next(t, "hello")
	dev.send(t, `{"t":"hello","proto":1,"fw":"0.2.0"}`) // its answer
	dev.none(t, "hello", 100*time.Millisecond)
	if u := m.Status().USB; u.State != StateReady {
		t.Fatalf("usb = %+v", u)
	}
}

func TestConfigureWiFi(t *testing.T) {
	m, devices := newUSBTestManager(t, io.Discard)
	if _, err := m.ConfigureWiFi(WiFiConfig{SSID: "home", Pass: "12345678"}); err == nil || !strings.Contains(err.Error(), "USB") {
		t.Fatalf("before linking: %v", err)
	}
	_ = devices

	var netNews atomic.Int32
	m, dev, devices := usbScreen(t, func() { netNews.Add(1) })
	m.wifiTimeout = 300 * time.Millisecond
	if _, err := m.ConfigureWiFi(WiFiConfig{SSID: "home", Pass: "short"}); err == nil {
		t.Fatal("bad password passed")
	}
	type result struct {
		try int
		err error
	}
	configure := func(w WiFiConfig) chan result {
		ch := make(chan result, 1)
		go func() {
			try, err := m.ConfigureWiFi(w)
			ch <- result{try, err}
		}()
		return ch
	}

	r := configure(WiFiConfig{SSID: "home", Pass: "12345678", IP: "192.168.1.50"})
	w := dev.next(t, "wifi")
	if w["ssid"] != "home" || w["pass"] != "12345678" || w["ip"] != "192.168.1.50" || w["mask"] != "255.255.255.0" {
		t.Fatalf("wifi = %v", w)
	}
	// News the screen sent before it read the settings are no answer, even
	// about the same network.
	dev.send(t, `{"t":"net","state":"failed","ssid":"home","port":47322,"code":"123456","fixed":true,"err":"密码不对"}`)
	waitFor(t, "old news", func() bool { n := m.Status().USB.Net; return n != nil && n.State == "failed" })
	if n := m.Status().USB.Net; n.Try != 0 {
		t.Fatalf("old news numbered %d", n.Try)
	}
	before := netNews.Load()
	dev.send(t, `{"t":"net","state":"connecting","ssid":"home","port":47322,"code":"123456","fixed":true}`)
	if got := <-r; got.err != nil || got.try != 1 {
		t.Fatalf("configure = %+v", got)
	}
	// The status has the answer by the time the caller hears it.
	if n := m.Status().USB.Net; n.State != "connecting" || n.Err != "" || n.Try != 1 {
		t.Fatalf("net = %+v", n)
	}
	dev.send(t, `{"t":"net","state":"connected","ssid":"home","ip":"192.168.1.50","port":47322,"code":"123456","fixed":true}`)
	waitFor(t, "joined", func() bool { return m.Status().USB.Net.State == "connected" })
	if n := *m.Status().USB.Net; n != (NetStatus{State: "connected", SSID: "home", IP: "192.168.1.50", Port: 47322, Code: "123456", Fixed: true, Try: 1}) || n.Addr() != "192.168.1.50" {
		t.Fatalf("net = %+v", n)
	}
	if netNews.Load() == before {
		t.Fatal("web not told")
	}

	// Settings the screen will not take come back as a warning, then the
	// WiFi as it was.
	r = configure(WiFiConfig{SSID: "cafe"})
	dev.next(t, "wifi")
	dev.send(t, `{"t":"log","level":"warn","msg":"正在扫描 WiFi，请几秒后再试"}`)
	dev.send(t, `{"t":"net","state":"connected","ssid":"home","ip":"192.168.1.50","port":47322,"code":"123456","fixed":true}`)
	if got := <-r; got.err == nil || got.err.Error() != "正在扫描 WiFi，请几秒后再试" {
		t.Fatalf("refused: %+v", got)
	}

	// No answer: the caller hears so.
	r = configure(WiFiConfig{SSID: "cafe"})
	dev.next(t, "wifi")
	if got := <-r; got.err == nil || !strings.Contains(got.err.Error(), "没有回复") {
		t.Fatalf("silent screen: %+v", got)
	}

	// WiFi off is answered by "off".
	r = configure(WiFiConfig{})
	if w := dev.next(t, "wifi"); w["ssid"] != "" {
		t.Fatalf("off = %v", w)
	}
	dev.send(t, `{"t":"net","state":"off","port":47322,"code":"123456"}`)
	if got := <-r; got.err != nil || got.try != 2 {
		t.Fatalf("off: %+v", got)
	}

	// Unplugged while waiting: the caller hears so at once, and what the
	// screen said about its WiFi goes with it.
	r = configure(WiFiConfig{SSID: "home", Pass: "12345678"})
	dev.next(t, "wifi")
	dev.conn.Close()
	if got := <-r; got.err == nil || !strings.Contains(got.err.Error(), "USB 连接中断") {
		t.Fatalf("unplugged: %+v", got)
	}
	<-devices
	waitFor(t, "forgotten", func() bool { u := m.Status().USB; return u.Net == nil && u.State != StateReady })
}

// The line with a WiFi password must fit, like every other line.
func TestWiFiLineFits(t *testing.T) {
	w := WiFiConfig{SSID: strings.Repeat("网", 10), Pass: strings.Repeat("\"", 63), IP: "255.255.255.255", Mask: "255.255.255.255", GW: "255.255.255.255"}
	if n := len(EncodeWiFi(w)); n > 512 {
		t.Fatalf("wifi line %d bytes", n)
	}
}

func TestScanWiFi(t *testing.T) {
	m, dev, _ := usbScreen(t, nil)
	m.scanTimeout = 300 * time.Millisecond

	type result struct {
		aps []AP
		err error
	}
	scan := func() chan result {
		ch := make(chan result, 1)
		go func() {
			aps, err := m.ScanWiFi(context.Background())
			ch <- result{aps, err}
		}()
		return ch
	}

	r := scan()
	dev.next(t, "wifi_scan")
	dev.send(t, `{"t":"wifi_list","aps":[{"ssid":"home","rssi":-48},{"ssid":"cafe","rssi":-80,"open":true}]}`)
	got := <-r
	if got.err != nil || len(got.aps) != 2 || got.aps[0] != (AP{SSID: "home", RSSI: -48}) || got.aps[1] != (AP{SSID: "cafe", RSSI: -80, Open: true}) {
		t.Fatalf("scan = %+v, %v", got.aps, got.err)
	}

	// Nothing in range is an empty list, not null.
	r = scan()
	dev.next(t, "wifi_scan")
	dev.send(t, `{"t":"wifi_list","aps":[]}`)
	if got := <-r; got.err != nil || got.aps == nil || len(got.aps) != 0 {
		t.Fatalf("empty scan = %#v, %v", got.aps, got.err)
	}

	r = scan()
	dev.next(t, "wifi_scan")
	dev.send(t, `{"t":"wifi_list","aps":[],"err":"现在不能扫描，请稍后再试"}`)
	if got := <-r; got.err == nil || got.err.Error() != "现在不能扫描，请稍后再试" {
		t.Fatalf("failed scan: %v", got.err)
	}

	// No answer: the caller hears so, and the late answer does not stand
	// in for the next scan.
	r = scan()
	dev.next(t, "wifi_scan")
	if got := <-r; got.err == nil || !strings.Contains(got.err.Error(), "没有回复") {
		t.Fatalf("silent screen: %v", got.err)
	}
	dev.send(t, `{"t":"wifi_list","aps":[{"ssid":"stale","rssi":-90}]}`)
	time.Sleep(50 * time.Millisecond)
	r = scan()
	dev.next(t, "wifi_scan")
	dev.send(t, `{"t":"wifi_list","aps":[{"ssid":"fresh","rssi":-40}]}`)
	if got := <-r; got.err != nil || len(got.aps) != 1 || got.aps[0].SSID != "fresh" {
		t.Fatalf("after a late answer: %+v, %v", got.aps, got.err)
	}
}

func TestScanWiFiWithoutCable(t *testing.T) {
	m, devices, _ := newNetManager(t, io.Discard)
	runManager(t, m)
	dev := <-devices
	dev.send(t, `{"t":"hello","proto":1,"fw":"0.2.0"}`)
	waitFor(t, "ready", func() bool { return m.Status().State == StateReady })
	if _, err := m.ScanWiFi(context.Background()); err == nil || !strings.Contains(err.Error(), "USB") {
		t.Fatalf("scan without the cable: %v", err)
	}
}
