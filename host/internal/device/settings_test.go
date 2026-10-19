package device

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestManagerSettings(t *testing.T) {
	const (
		helloPlain    = `{"t":"hello","proto":1,"fw":"0.1.0","w":600,"h":450,"max_line":4096}`
		helloSettings = `{"t":"hello","proto":1,"fw":"0.3.0","w":600,"h":450,"max_line":4096,"settings":1}`
	)
	src := &fakeSource{snap: sampleSnapshot(), ch: make(chan struct{}, 1)}
	m, devices := newTestManager(t, src, io.Discard)
	opts := Options{Enabled: true, Port: "auto", MaxSessions: 12, Brightness: 80, DimBrightness: 20, DimAfter: 5 * time.Minute}
	m.opts = opts
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	dev := <-devices
	if err := m.SettingsReady(); err != nil {
		t.Fatalf("before hello: %v", err) // saved now, sent on connect
	}
	// Firmware without settings gets none.
	dev.send(t, helloPlain)
	waitFor(t, "ready", func() bool { return m.Status().State == StateReady })
	dev.none(t, "settings", 100*time.Millisecond)
	if err := m.SettingsReady(); err == nil || !strings.Contains(err.Error(), "升级固件") {
		t.Fatalf("old firmware: %v", err)
	}

	// With them, the handshake sends the current ones.
	m.Reconnect()
	dev = <-devices
	dev.send(t, helloSettings)
	msg := dev.next(t, "settings")
	if msg["brightness"] != 80.0 || msg["dim_brightness"] != 20.0 || msg["dim_after"] != 300.0 {
		t.Fatalf("settings %v", msg)
	}
	if err := m.SettingsReady(); err != nil || !m.Status().Settings {
		t.Fatalf("ready %v, status %+v", err, m.Status())
	}

	// A change goes out at once; other options do not resend them.
	opts.Brightness, opts.DimAfter = 50, 0
	m.SetOptions(opts)
	msg = dev.next(t, "settings")
	if msg["brightness"] != 50.0 || msg["dim_after"] != 0.0 {
		t.Fatalf("changed %v", msg)
	}
	opts.ShowIdle = true
	m.SetOptions(opts)
	dev.none(t, "settings", 100*time.Millisecond)
}
