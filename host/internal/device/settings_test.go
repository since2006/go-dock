package device

import (
	"context"
	"io"
	"testing"
	"time"
)

func TestManagerSettings(t *testing.T) {
	src := &fakeSource{snap: sampleSnapshot(), ch: make(chan struct{}, 1)}
	m, devices := newTestManager(t, src, io.Discard)
	opts := testOptions()
	opts.Brightness, opts.DimBrightness, opts.DimAfter = 80, 20, 5*time.Minute
	m.opts = opts
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	// The handshake sends the current settings.
	dev := <-devices
	dev.send(t, `{"t":"hello","proto":1,"fw":"0.3.0","w":600,"h":450}`)
	msg := dev.next(t, "settings")
	if msg["brightness"] != 80.0 || msg["dim_brightness"] != 20.0 || msg["dim_after"] != 300.0 {
		t.Fatalf("settings %v", msg)
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
