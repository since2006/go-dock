// Package mqtttest is a small in-process MQTT 3.1.1 broker for tests:
// enough of the protocol for go-dock's client (QoS 0/1, retained messages,
// wildcards), plus hooks to inspect traffic and to break connections.
package mqtttest

import (
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/eclipse/paho.mqtt.golang/packets"
)

// Msg is a message a client published.
type Msg struct {
	Topic   string
	Payload string
	Retain  bool
}

type Broker struct {
	ln net.Listener

	received     chan Msg
	subscribed   chan string
	unsubscribed chan string

	mu       sync.Mutex
	user     string
	pass     string
	auth     bool
	conns    map[*conn]bool
	retained map[string][]byte
	connects int
}

type conn struct {
	nc   net.Conn
	wmu  sync.Mutex
	subs map[string]bool // guarded by Broker.mu
}

// New starts a broker on a loopback port; it stops when the test ends.
func New(t testing.TB) *Broker {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &Broker{
		ln:           ln,
		received:     make(chan Msg, 256),
		subscribed:   make(chan string, 256),
		unsubscribed: make(chan string, 256),
		conns:        map[*conn]bool{},
		retained:     map[string][]byte{},
	}
	go b.accept()
	t.Cleanup(b.Close)
	return b
}

// URL is the broker address for mqtt.Options.Broker.
func (b *Broker) URL() string { return "mqtt://" + b.ln.Addr().String() }

// RequireAuth rejects clients that do not log in with user and pass.
func (b *Broker) RequireAuth(user, pass string) {
	b.mu.Lock()
	b.user, b.pass, b.auth = user, pass, true
	b.mu.Unlock()
}

// Received yields every message clients publish.
func (b *Broker) Received() <-chan Msg { return b.received }

// Subscribed yields every filter clients subscribe to.
func (b *Broker) Subscribed() <-chan string { return b.subscribed }

// Unsubscribed yields every filter clients unsubscribe from.
func (b *Broker) Unsubscribed() <-chan string { return b.unsubscribed }

// Connects counts accepted connections.
func (b *Broker) Connects() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connects
}

// Publish delivers a message to subscribed clients, as if a device sent it.
func (b *Broker) Publish(topic, payload string, retain bool) {
	b.route(topic, []byte(payload), retain)
}

// Kill drops every client connection, like a broker restart.
func (b *Broker) Kill() {
	b.mu.Lock()
	cs := make([]*conn, 0, len(b.conns))
	for c := range b.conns {
		cs = append(cs, c)
	}
	b.mu.Unlock()
	for _, c := range cs {
		c.nc.Close()
	}
}

// Close stops the broker.
func (b *Broker) Close() {
	b.ln.Close()
	b.Kill()
}

func (b *Broker) accept() {
	for {
		nc, err := b.ln.Accept()
		if err != nil {
			return
		}
		go b.serve(&conn{nc: nc, subs: map[string]bool{}})
	}
}

func (c *conn) send(p packets.ControlPacket) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := p.Write(c.nc); err != nil {
		c.nc.Close()
	}
}

func (b *Broker) serve(c *conn) {
	defer c.nc.Close()
	p, err := packets.ReadPacket(c.nc)
	if err != nil {
		return
	}
	cp, ok := p.(*packets.ConnectPacket)
	if !ok {
		return
	}
	ack := packets.NewControlPacket(packets.Connack).(*packets.ConnackPacket)
	b.mu.Lock()
	if b.auth && (cp.Username != b.user || string(cp.Password) != b.pass) {
		ack.ReturnCode = packets.ErrRefusedBadUsernameOrPassword
	}
	if ack.ReturnCode == packets.Accepted {
		b.conns[c] = true
		b.connects++
	}
	b.mu.Unlock()
	c.send(ack)
	if ack.ReturnCode != packets.Accepted {
		return
	}
	defer func() {
		b.mu.Lock()
		delete(b.conns, c)
		b.mu.Unlock()
	}()

	for {
		p, err := packets.ReadPacket(c.nc)
		if err != nil {
			return
		}
		switch p := p.(type) {
		case *packets.SubscribePacket:
			sa := packets.NewControlPacket(packets.Suback).(*packets.SubackPacket)
			sa.MessageID = p.MessageID
			var retained []Msg
			b.mu.Lock()
			for i, f := range p.Topics {
				c.subs[f] = true
				sa.ReturnCodes = append(sa.ReturnCodes, min(p.Qoss[i], 1))
				for t, v := range b.retained {
					if Match(f, t) {
						retained = append(retained, Msg{Topic: t, Payload: string(v), Retain: true})
					}
				}
			}
			b.mu.Unlock()
			c.send(sa)
			for _, f := range p.Topics {
				put(b.subscribed, f)
			}
			for _, m := range retained {
				c.send(publish(m.Topic, []byte(m.Payload), true))
			}
		case *packets.UnsubscribePacket:
			b.mu.Lock()
			for _, f := range p.Topics {
				delete(c.subs, f)
			}
			b.mu.Unlock()
			ua := packets.NewControlPacket(packets.Unsuback).(*packets.UnsubackPacket)
			ua.MessageID = p.MessageID
			c.send(ua)
			for _, f := range p.Topics {
				put(b.unsubscribed, f)
			}
		case *packets.PublishPacket:
			if p.Qos == 1 {
				pa := packets.NewControlPacket(packets.Puback).(*packets.PubackPacket)
				pa.MessageID = p.MessageID
				c.send(pa)
			}
			put(b.received, Msg{Topic: p.TopicName, Payload: string(p.Payload), Retain: p.Retain})
			b.route(p.TopicName, p.Payload, p.Retain)
		case *packets.PingreqPacket:
			c.send(packets.NewControlPacket(packets.Pingresp))
		case *packets.DisconnectPacket:
			return
		}
	}
}

func (b *Broker) route(topic string, payload []byte, retain bool) {
	b.mu.Lock()
	if retain {
		if len(payload) == 0 {
			delete(b.retained, topic)
		} else {
			b.retained[topic] = payload
		}
	}
	var to []*conn
	for c := range b.conns {
		for f := range c.subs {
			if Match(f, topic) {
				to = append(to, c)
				break
			}
		}
	}
	b.mu.Unlock()
	for _, c := range to {
		c.send(publish(topic, payload, false))
	}
}

// put records without ever blocking the connection; tests read far fewer
// than the buffer holds.
func put[T any](ch chan T, v T) {
	select {
	case ch <- v:
	default:
	}
}

func publish(topic string, payload []byte, retain bool) *packets.PublishPacket {
	p := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
	p.TopicName, p.Payload, p.Retain = topic, payload, retain
	return p
}

// Match reports whether topic matches the subscription filter, with the
// + and # wildcards.
func Match(filter, topic string) bool {
	fs, ts := strings.Split(filter, "/"), strings.Split(topic, "/")
	for i, f := range fs {
		if f == "#" {
			return true
		}
		if i >= len(ts) || (f != "+" && f != ts[i]) {
			return false
		}
	}
	return len(fs) == len(ts)
}
