package mqtt

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/since2006/go-dock/host/internal/mqtt/mqtttest"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newTestClient(t *testing.T, o Options) (*Client, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	c := New(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), nil)
	c.retryMin, c.retryMax, c.opTimeout = 20*time.Millisecond, 100*time.Millisecond, time.Second
	c.SetOptions(o)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return c, logs
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func next[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("timed out")
	}
	var zero T
	return zero
}

func TestSubscribePublishAndReconnect(t *testing.T) {
	b := mqtttest.New(t)
	b.RequireAuth("u", "secret-pass")
	c, logs := newTestClient(t, Options{Enabled: true, Broker: b.URL(), Username: "u", Password: "secret-pass", ClientID: "test"})

	got := make(chan string, 8)
	c.Subscribe("office/light/state", func(topic string, p []byte) { got <- "a:" + string(p) })
	unsubB := c.Subscribe("office/light/state", func(topic string, p []byte) { got <- "b:" + string(p) })
	connects := make(chan struct{}, 4)
	c.OnConnect(func() { connects <- struct{}{} })

	waitFor(t, "connected", c.Connected)
	if st := c.Status(); st.State != StateConnected || st.ClientID != "test" || st.Broker != b.URL() {
		t.Fatalf("status %+v", st)
	}
	if f := next(t, b.Subscribed()); f != "office/light/state" {
		t.Fatalf("subscribed %q", f)
	}
	next(t, connects)

	b.Publish("office/light/state", "ON", false)
	seen := map[string]bool{next(t, got): true, next(t, got): true}
	if !seen["a:ON"] || !seen["b:ON"] {
		t.Fatalf("handlers got %v", seen)
	}

	// Removing one of two handlers keeps the subscription.
	unsubB()
	b.Publish("office/light/state", "OFF", false)
	if v := next(t, got); v != "a:OFF" {
		t.Fatalf("got %q", v)
	}
	select {
	case v := <-got:
		t.Fatalf("removed handler still called: %q", v)
	case <-time.After(50 * time.Millisecond):
	}

	if err := c.Publish(context.Background(), "office/light/set", []byte("ON"), false); err != nil {
		t.Fatal(err)
	}
	if m := next(t, b.Received()); m.Topic != "office/light/set" || m.Payload != "ON" {
		t.Fatalf("received %+v", m)
	}

	// A broker restart: the client reconnects, subscribes again and runs
	// the connect hooks again.
	b.Kill()
	if f := next(t, b.Subscribed()); f != "office/light/state" {
		t.Fatalf("resubscribed %q", f)
	}
	next(t, connects)
	waitFor(t, "reconnected", c.Connected)
	b.Publish("office/light/state", "ON", false)
	if v := next(t, got); v != "a:ON" {
		t.Fatalf("after reconnect got %q", v)
	}
	if b.Connects() != 2 {
		t.Fatalf("connects = %d", b.Connects())
	}
	if strings.Contains(logs.String(), "secret-pass") {
		t.Fatal("password logged")
	}
}

func TestUnsubscribeLast(t *testing.T) {
	b := mqtttest.New(t)
	c, _ := newTestClient(t, Options{Enabled: true, Broker: b.URL()})
	unsub := c.Subscribe("a/b", func(string, []byte) {})
	waitFor(t, "connected", c.Connected)
	next(t, b.Subscribed())
	unsub()
	unsub() // idempotent
	if f := next(t, b.Unsubscribed()); f != "a/b" {
		t.Fatalf("unsubscribed %q", f)
	}
	// Subscribing while connected goes out at once.
	c.Subscribe("c/d", func(string, []byte) {})
	if f := next(t, b.Subscribed()); f != "c/d" {
		t.Fatalf("subscribed %q", f)
	}
}

func TestBadPasswordRetries(t *testing.T) {
	b := mqtttest.New(t)
	b.RequireAuth("u", "right")
	c, _ := newTestClient(t, Options{Enabled: true, Broker: b.URL(), Username: "u", Password: "wrong"})
	waitFor(t, "error", func() bool { return c.Status().State == StateError })
	if st := c.Status(); st.Error != "用户名或密码错误" {
		t.Fatalf("error %q", st.Error)
	}
	if err := c.Publish(context.Background(), "x", nil, false); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("publish while down: %v", err)
	}
	// Fixing the password reconnects without waiting for the backoff.
	c.SetOptions(Options{Enabled: true, Broker: b.URL(), Username: "u", Password: "right"})
	waitFor(t, "connected", c.Connected)
}

func TestUnreachableBroker(t *testing.T) {
	b := mqtttest.New(t)
	url := b.URL()
	b.Close()
	c, logs := newTestClient(t, Options{Enabled: true, Broker: url})
	waitFor(t, "error", func() bool { return c.Status().State == StateError })
	if st := c.Status(); !strings.HasPrefix(st.Error, "连不上 broker") {
		t.Fatalf("error %q", st.Error)
	}
	time.Sleep(150 * time.Millisecond) // a few retries
	if n := strings.Count(logs.String(), "MQTT 连接失败"); n != 1 {
		t.Fatalf("logged %d times, want once while the error stays the same", n)
	}
}

func TestSetOptionsAndDisable(t *testing.T) {
	b := mqtttest.New(t)
	o := Options{Enabled: true, Broker: b.URL()}
	c, _ := newTestClient(t, o)
	waitFor(t, "connected", c.Connected)

	c.SetOptions(o) // unchanged: no reconnect
	time.Sleep(50 * time.Millisecond)
	if b.Connects() != 1 {
		t.Fatalf("connects = %d after unchanged options", b.Connects())
	}

	o.ClientID = "other"
	c.SetOptions(o)
	waitFor(t, "reconnect", func() bool { return b.Connects() == 2 && c.Connected() })
	if c.Status().ClientID != "other" {
		t.Fatalf("status %+v", c.Status())
	}

	c.SetOptions(Options{Broker: b.URL()})
	waitFor(t, "disabled", func() bool { return c.Status().State == StateDisabled })
	if c.Connected() {
		t.Fatal("still connected")
	}
}

func TestDefaultClientID(t *testing.T) {
	id := defaultClientID()
	if !strings.HasPrefix(id, "go-dock-") || len(id) > 23 {
		t.Fatalf("id %q", id)
	}
	if id == defaultClientID() {
		t.Fatal("ids repeat")
	}
}
