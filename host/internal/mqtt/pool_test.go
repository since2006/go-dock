package mqtt

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/since2006/go-dock/host/internal/mqtt/mqtttest"
)

func TestPool(t *testing.T) {
	office, home := mqtttest.New(t), mqtttest.New(t)
	p := NewPool(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	brokers := []BrokerOptions{
		{ID: "office", Name: "办公室", Options: Options{Enabled: true, Broker: office.URL()}},
		{ID: "home", Name: "家", Options: Options{Enabled: true, Broker: home.URL()}},
	}
	p.SetOptions(brokers)
	// A client handed out before Run starts with it.
	got := make(chan string, 4)
	p.Client("home").Subscribe("home/lamp", func(_ string, b []byte) { got <- string(b) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	waitFor(t, "both connected", func() bool { return p.Client("office").Connected() && p.Client("home").Connected() })
	if p.Client("office") == p.Client("home") || p.Client("office") != p.Client("office") {
		t.Fatal("one client per broker id expected")
	}
	st := p.Status()
	if len(st) != 2 || st[0].ID != "office" || st[0].Name != "办公室" || st[1].State != StateConnected {
		t.Fatalf("status %+v", st)
	}

	// Each broker gets only its own traffic.
	next(t, home.Subscribed())
	home.Publish("home/lamp", "ON", false)
	if v := next(t, got); v != "ON" {
		t.Fatalf("got %q", v)
	}
	if err := p.Client("office").Publish(context.Background(), "office/lamp", []byte("OFF"), false); err != nil {
		t.Fatal(err)
	}
	if m := next(t, office.Received()); m.Topic != "office/lamp" {
		t.Fatalf("office received %+v", m)
	}

	// Removing a broker disables its client but keeps the handle; adding
	// it back reconnects with the old subscriptions.
	p.SetOptions(brokers[:1])
	waitFor(t, "home disabled", func() bool { return p.Client("home").Status().State == StateDisabled })
	if st := p.Status(); len(st) != 1 {
		t.Fatalf("status %+v", st)
	}
	p.SetOptions(brokers)
	waitFor(t, "home back", p.Client("home").Connected)
	next(t, home.Subscribed())
	home.Publish("home/lamp", "OFF", false)
	if v := next(t, got); v != "OFF" {
		t.Fatalf("got %q", v)
	}

	// A client asked for after Run starts runs too.
	late := mqtttest.New(t)
	p.SetOptions(append(brokers, BrokerOptions{ID: "late", Options: Options{Enabled: true, Broker: late.URL()}}))
	waitFor(t, "late connected", p.Client("late").Connected)
}
