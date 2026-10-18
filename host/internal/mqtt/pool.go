package mqtt

import (
	"context"
	"log/slog"
	"sync"
)

// BrokerOptions configures one broker of a Pool.
type BrokerOptions struct {
	ID   string
	Name string
	Options
}

// BrokerStatus is one broker's state for the web UI.
type BrokerStatus struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Status
}

// Pool keeps one Client per configured broker. A client lives as long as
// the pool: removing a broker only disables it, so a plugin holding it
// keeps a valid handle and its subscriptions come back if the broker does.
type Pool struct {
	log      *slog.Logger
	onChange func()

	mu      sync.Mutex
	ctx     context.Context // set by Run; clients start with it
	clients map[string]*Client
	brokers []BrokerOptions // configured, in order
}

func NewPool(log *slog.Logger, onChange func()) *Pool {
	return &Pool{log: log, onChange: onChange, clients: map[string]*Client{}}
}

// Client returns the client for broker id, creating a disabled one if the
// broker is not configured (yet).
func (p *Pool) Client(id string) *Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.client(id)
}

func (p *Pool) client(id string) *Client {
	if c, ok := p.clients[id]; ok {
		return c
	}
	c := New(p.log.With("broker_id", id), p.onChange)
	p.clients[id] = c
	if p.ctx != nil {
		go c.Run(p.ctx)
	}
	return c
}

// SetOptions applies the broker list; each client reconnects only when its
// own options changed.
func (p *Pool) SetOptions(bs []BrokerOptions) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.brokers = append([]BrokerOptions(nil), bs...)
	seen := map[string]bool{}
	for _, b := range bs {
		seen[b.ID] = true
		p.client(b.ID).SetOptions(b.Options)
	}
	for id, c := range p.clients {
		if !seen[id] {
			c.SetOptions(Options{})
		}
	}
}

// Status lists the configured brokers in order.
func (p *Pool) Status() []BrokerStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]BrokerStatus, 0, len(p.brokers))
	for _, b := range p.brokers {
		out = append(out, BrokerStatus{ID: b.ID, Name: b.Name, Status: p.clients[b.ID].Status()})
	}
	return out
}

// Run runs every client until ctx ends.
func (p *Pool) Run(ctx context.Context) {
	p.mu.Lock()
	p.ctx = ctx
	for _, c := range p.clients {
		go c.Run(ctx)
	}
	p.mu.Unlock()
	<-ctx.Done()
}
