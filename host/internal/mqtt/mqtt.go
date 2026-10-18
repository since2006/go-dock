// Package mqtt keeps one connection to an MQTT broker for the plugins:
// subscriptions survive reconnects, and publishing fails fast while the
// broker is unreachable instead of queueing stale commands.
package mqtt

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"
)

// Options configures the broker connection.
type Options struct {
	Enabled bool
	// Broker is a URL normalized by config.BrokerURL.
	Broker   string
	Username string
	Password string
	// ClientID defaults to go-dock-<host>-<random>.
	ClientID string
}

// Connection states reported in Status.State.
const (
	StateDisabled   = "disabled"
	StateConnecting = "connecting"
	StateConnected  = "connected"
	StateError      = "error" // the last attempt failed; retrying
)

// Status is the connection state shown on the web UI.
type Status struct {
	State    string    `json:"state"`
	Broker   string    `json:"broker,omitempty"`
	ClientID string    `json:"client_id,omitempty"`
	Error    string    `json:"error,omitempty"`
	Since    time.Time `json:"since,omitzero"`
}

// ErrNotConnected is returned by Publish while there is no connection.
var ErrNotConnected = errors.New("MQTT 未连接")

var errKicked = errors.New("options changed")

// Client is the shared broker connection.
type Client struct {
	log       *slog.Logger
	onChange  func()
	defaultID string

	// Replaceable in tests.
	retryMin, retryMax, opTimeout time.Duration

	kick chan struct{}

	// netMu orders subscription changes on the wire with the registry, so
	// a quick unsubscribe and subscribe of one filter cannot swap.
	netMu sync.Mutex
	mu    sync.Mutex
	opts  Options
	st    Status
	conn  paho.Client // set while connected
	subs  map[string][]*handler
	hooks []*hook
}

type handler struct {
	f func(topic string, payload []byte)
}

type hook struct{ f func() }

// New creates a disabled client; SetOptions configures it and Run
// connects. onChange, if set, is called whenever Status changes.
func New(log *slog.Logger, onChange func()) *Client {
	if onChange == nil {
		onChange = func() {}
	}
	return &Client{
		log:       log.With("component", "mqtt"),
		onChange:  onChange,
		defaultID: defaultClientID(),
		retryMin:  time.Second,
		retryMax:  30 * time.Second,
		opTimeout: 5 * time.Second,
		kick:      make(chan struct{}, 1),
		st:        Status{State: StateDisabled},
		subs:      map[string][]*handler{},
	}
}

// defaultClientID is unique per process, so two go-dock instances on one
// machine do not keep kicking each other off the broker. It stays within
// the 23 bytes every broker accepts.
func defaultClientID() string {
	host, _ := os.Hostname()
	host = strings.Map(func(r rune) rune {
		if r < 128 && (r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return r
		}
		return -1
	}, host)
	if len(host) > 8 {
		host = host[:8]
	}
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	if host == "" {
		return "go-dock-" + hex.EncodeToString(b)
	}
	return "go-dock-" + host + "-" + hex.EncodeToString(b)
}

// SetOptions applies new options, reconnecting only when they changed.
func (c *Client) SetOptions(o Options) {
	c.mu.Lock()
	changed := c.opts != o
	c.opts = o
	c.mu.Unlock()
	if changed {
		select {
		case c.kick <- struct{}{}:
		default:
		}
	}
}

func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st
}

// Connected reports whether a publish can go out right now.
func (c *Client) Connected() bool {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	return conn != nil && conn.IsConnectionOpen()
}

func (c *Client) setStatus(st Status) {
	c.mu.Lock()
	changed := c.st != st
	c.st = st
	c.mu.Unlock()
	if changed {
		c.onChange()
	}
}

// Subscribe calls h for every message matching filter, across reconnects,
// until unsubscribe is called. h runs on the client's receive goroutine, in
// message order: it must not block or publish.
func (c *Client) Subscribe(filter string, h func(topic string, payload []byte)) (unsubscribe func()) {
	s := &handler{f: h}
	c.netMu.Lock()
	c.mu.Lock()
	first := len(c.subs[filter]) == 0
	c.subs[filter] = append(c.subs[filter], s)
	conn := c.conn
	c.mu.Unlock()
	if first && conn != nil {
		c.subscribe(conn, filter)
	}
	c.netMu.Unlock()

	var once sync.Once
	return func() { once.Do(func() { c.unsubscribe(filter, s) }) }
}

func (c *Client) unsubscribe(filter string, s *handler) {
	c.netMu.Lock()
	defer c.netMu.Unlock()
	c.mu.Lock()
	hs := slices.DeleteFunc(slices.Clone(c.subs[filter]), func(x *handler) bool { return x == s })
	if len(hs) > 0 {
		c.subs[filter] = hs
	} else {
		delete(c.subs, filter)
	}
	conn := c.conn
	c.mu.Unlock()
	if len(hs) == 0 && conn != nil {
		c.await(conn.Unsubscribe(filter), "取消订阅", filter)
	}
}

// subscribe sends one SUBSCRIBE; the caller holds netMu.
func (c *Client) subscribe(conn paho.Client, filter string) {
	tok := conn.Subscribe(filter, 1, c.route(filter))
	go func() {
		if !tok.WaitTimeout(c.opTimeout) {
			c.log.Warn("MQTT 订阅超时", "topic", filter)
			return
		}
		if err := tok.Error(); err != nil {
			c.log.Warn("MQTT 订阅失败", "topic", filter, "err", err)
			return
		}
		if st, ok := tok.(*paho.SubscribeToken); ok && st.Result()[filter] == 0x80 {
			c.log.Warn("MQTT broker 拒绝订阅（检查账号权限）", "topic", filter)
		}
	}()
}

func (c *Client) await(tok paho.Token, what, topic string) {
	go func() {
		if tok.WaitTimeout(c.opTimeout) && tok.Error() != nil {
			c.log.Debug("MQTT "+what+"失败", "topic", topic, "err", tok.Error())
		}
	}()
}

// route hands a message to the handlers registered for filter now.
func (c *Client) route(filter string) paho.MessageHandler {
	return func(_ paho.Client, m paho.Message) {
		c.mu.Lock()
		hs := c.subs[filter]
		c.mu.Unlock()
		c.log.Debug("MQTT 收到消息", "topic", m.Topic(), "payload", string(m.Payload()))
		for _, h := range hs {
			h.f(m.Topic(), m.Payload())
		}
	}
}

// OnConnect runs f in its own goroutine after every successful connect,
// once the subscriptions are sent; plugins use it to ask devices for their
// current state.
func (c *Client) OnConnect(f func()) (remove func()) {
	h := &hook{f: f}
	c.mu.Lock()
	c.hooks = append(c.hooks, h)
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		c.hooks = slices.DeleteFunc(slices.Clone(c.hooks), func(x *hook) bool { return x == h })
		c.mu.Unlock()
	}
}

// Publish sends payload at QoS 0. It fails at once while disconnected, so
// a command never goes out late.
func (c *Client) Publish(ctx context.Context, topic string, payload []byte, retain bool) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil || !conn.IsConnectionOpen() {
		return ErrNotConnected
	}
	tok := conn.Publish(topic, 0, retain, payload)
	t := time.NewTimer(c.opTimeout)
	defer t.Stop()
	select {
	case <-tok.Done():
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return errors.New("MQTT 发送超时")
	}
	if err := tok.Error(); err != nil {
		return fmt.Errorf("MQTT 发送失败: %w", err)
	}
	c.log.Debug("MQTT 已发送", "topic", topic, "payload", string(payload))
	return nil
}

func (c *Client) options() Options {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opts
}

// Run keeps the connection up until ctx ends.
func (c *Client) Run(ctx context.Context) {
	backoff := c.retryMin
	lastErr := ""
	for ctx.Err() == nil {
		// The options read next are the latest; a kick from before is stale.
		select {
		case <-c.kick:
		default:
		}
		o := c.options()
		id := o.ClientID
		if id == "" {
			id = c.defaultID
		}
		if !o.Enabled || o.Broker == "" {
			lastErr = ""
			c.setStatus(Status{State: StateDisabled})
			c.wait(ctx, 0)
			continue
		}
		if st := c.Status(); st.State != StateError || st.Broker != o.Broker {
			c.setStatus(Status{State: StateConnecting, Broker: o.Broker, ClientID: id, Since: time.Now()})
		}
		conn, lost, err := c.connect(ctx, o, id)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, errKicked):
			backoff, lastErr = c.retryMin, ""
			continue
		case err != nil:
			if msg := err.Error(); msg != lastErr {
				c.log.Warn("MQTT 连接失败", "broker", o.Broker, "err", msg)
				lastErr = msg
			}
			if st := c.Status(); st.State != StateError || st.Error != err.Error() {
				c.setStatus(Status{State: StateError, Broker: o.Broker, ClientID: id, Error: err.Error(), Since: time.Now()})
			}
			if c.wait(ctx, backoff) {
				backoff, lastErr = c.retryMin, ""
			} else {
				backoff = min(backoff*2, c.retryMax)
			}
			continue
		}
		backoff, lastErr = c.retryMin, ""
		c.log.Info("MQTT 已连接", "broker", o.Broker, "client_id", id)
		select {
		case <-ctx.Done():
			c.drop(conn)
			return
		case <-c.kick:
			c.drop(conn)
		case err := <-lost:
			c.drop(conn)
			msg := "连接断开"
			if err != nil {
				msg += ": " + err.Error()
			}
			c.log.Warn("MQTT 连接断开", "broker", o.Broker, "err", err)
			lastErr = msg
			c.setStatus(Status{State: StateError, Broker: o.Broker, ClientID: id, Error: msg, Since: time.Now()})
			if c.wait(ctx, c.retryMin) {
				lastErr = ""
			}
		}
	}
}

// wait sleeps for d (forever when d is 0) and reports whether the options
// changed meanwhile.
func (c *Client) wait(ctx context.Context, d time.Duration) (kicked bool) {
	var timer <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-ctx.Done():
	case <-c.kick:
		return true
	case <-timer:
	}
	return false
}

// connect makes one attempt. paho's own retry and reconnect are off: they
// cannot be stopped promptly when the options change or go-dock exits.
func (c *Client) connect(ctx context.Context, o Options, id string) (paho.Client, <-chan error, error) {
	lost := make(chan error, 1)
	po := paho.NewClientOptions().
		AddBroker(o.Broker).
		SetClientID(id).
		SetUsername(o.Username).
		SetPassword(o.Password).
		SetCleanSession(true).
		SetProtocolVersion(4).
		SetAutoReconnect(false).
		SetConnectRetry(false).
		SetKeepAlive(30 * time.Second).
		SetConnectTimeout(2 * c.opTimeout).
		SetWriteTimeout(c.opTimeout).
		SetDialer(&net.Dialer{Timeout: 2 * c.opTimeout}).
		SetConnectionLostHandler(func(_ paho.Client, err error) {
			select {
			case lost <- err:
			default:
			}
		})
	if u, err := url.Parse(o.Broker); err == nil && u.Scheme != "ws" && u.Scheme != "wss" {
		// paho would route plain and TLS brokers through $all_proxy, which
		// breaks LAN brokers whenever go-dock starts from a proxied shell.
		po.SetCustomOpenConnectionFn(c.dialDirect)
	}
	conn := paho.NewClient(po)
	tok := conn.Connect()
	select {
	case <-tok.Done():
	case <-ctx.Done():
		go abandon(conn, tok)
		return nil, nil, ctx.Err()
	case <-c.kick:
		go abandon(conn, tok)
		return nil, nil, errKicked
	}
	if err := tok.Error(); err != nil {
		return nil, nil, connectError(tok, err)
	}

	c.netMu.Lock()
	c.mu.Lock()
	c.conn = conn
	filters := slices.Sorted(maps.Keys(c.subs))
	hooks := slices.Clone(c.hooks)
	c.mu.Unlock()
	for _, f := range filters {
		c.subscribe(conn, f)
	}
	c.netMu.Unlock()
	c.setStatus(Status{State: StateConnected, Broker: o.Broker, ClientID: id, Since: time.Now()})
	for _, h := range hooks {
		go h.f()
	}
	return conn, lost, nil
}

func (c *Client) dialDirect(u *url.URL, o paho.ClientOptions) (net.Conn, error) {
	d := &net.Dialer{Timeout: 2 * c.opTimeout}
	switch u.Scheme {
	case "mqtt", "tcp":
		return d.Dial("tcp", u.Host)
	default:
		return tls.DialWithDialer(d, "tcp", u.Host, o.TLSConfig)
	}
}

// abandon disconnects an attempt nobody waits for once it settles.
func abandon(conn paho.Client, tok paho.Token) {
	<-tok.Done()
	if tok.Error() == nil {
		conn.Disconnect(0)
	}
}

func (c *Client) drop(conn paho.Client) {
	c.netMu.Lock()
	c.mu.Lock()
	c.conn = nil
	c.mu.Unlock()
	c.netMu.Unlock()
	conn.Disconnect(250)
}

// connectError explains a refused or failed connect in the UI's language.
func connectError(tok paho.Token, err error) error {
	if ct, ok := tok.(*paho.ConnectToken); ok {
		switch ct.ReturnCode() {
		case packets.ErrRefusedBadProtocolVersion:
			return errors.New("broker 不支持 MQTT 3.1.1")
		case packets.ErrRefusedIDRejected:
			return errors.New("broker 拒绝了这个 client id")
		case packets.ErrRefusedServerUnavailable:
			return errors.New("broker 暂时不可用")
		case packets.ErrRefusedBadUsernameOrPassword:
			return errors.New("用户名或密码错误")
		case packets.ErrRefusedNotAuthorised:
			return errors.New("没有连接权限")
		}
	}
	msg := strings.TrimPrefix(err.Error(), packets.ErrorNetworkError.Error()+" : ")
	return fmt.Errorf("连不上 broker: %s", msg)
}
