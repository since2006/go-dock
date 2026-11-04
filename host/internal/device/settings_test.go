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
	hello := func(dev *fakeDevice) {
		t.Helper()
		dev.next(t, "hello")
		dev.send(t, `{"t":"hello","proto":1,"fw":"0.3.0","w":600,"h":450}`)
	}

	// Settings left as they came do not go out on connecting: another
	// computer on the screen may have set its own.
	dev := <-devices
	hello(dev)
	dev.next(t, "snapshot")
	dev.none(t, "settings", 100*time.Millisecond)

	// A change goes out at once; other options do not resend them.
	opts.Brightness, opts.DimAfter = 50, 0
	opts.OwnSettings = true
	m.SetOptions(opts)
	msg := dev.next(t, "settings")
	if msg["brightness"] != 50.0 || msg["dim_brightness"] != 20.0 || msg["dim_after"] != 0.0 {
		t.Fatalf("changed %v", msg)
	}
	opts.ShowIdle = true
	m.SetOptions(opts)
	dev.none(t, "settings", 100*time.Millisecond)

	// The user's own settings go out on every connecting.
	dev.conn.Close()
	dev = <-devices
	hello(dev)
	if msg := dev.next(t, "settings"); msg["brightness"] != 50.0 {
		t.Fatalf("own settings on connecting: %v", msg)
	}

	// Back to the defaults while the screen was away: they go out once.
	opts.Brightness, opts.DimAfter, opts.OwnSettings = 80, 5*time.Minute, false
	dev.conn.Close()
	m.SetOptions(opts)
	dev = <-devices
	hello(dev)
	if msg := dev.next(t, "settings"); msg["brightness"] != 80.0 || msg["dim_after"] != 300.0 {
		t.Fatalf("changed while away: %v", msg)
	}
	dev.conn.Close()
	dev = <-devices
	hello(dev)
	dev.next(t, "snapshot")
	dev.none(t, "settings", 100*time.Millisecond)
}
