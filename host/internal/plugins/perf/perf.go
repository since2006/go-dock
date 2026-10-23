// Package perf is the 性能 page: the computer's CPU, GPU, memory, fans,
// network and disk, read every two seconds while the page is on. The page
// is a dashboard: the first Big cards are big (a chart of the last minute
// and what it is made of), the others small. Its cards take no taps, but
// the page is movable: a card dropped on another trades places with it,
// and the order is saved to the config. What the computer cannot report
// (a GPU's load, fans on Windows) is left off.
package perf

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/plugins"
	"github.com/since2006/go-dock/host/internal/sysmon"
)

// ID is the page id in the protocol.
const ID = "perf"

// Every is how often the page reads; the last History readings make a
// card's chart (a minute).
const (
	Every   = 2 * time.Second
	History = 30
)

// Big is how many cards, from the first, are drawn big.
const Big = 2

// Cards lists the cards in their usual order.
var Cards = []string{"cpu", "gpu", "mem", "fan", "net", "disk"}

// Reader takes samples; *sysmon.Collector implements it.
type Reader interface {
	Read(ctx context.Context) sysmon.Sample
	Model() string
}

// Save stores a new card order in the config.
type Save func(order []string) error

type Options struct {
	Enabled bool
	Order   []string // card ids, as Order reads them
}

type Plugin struct {
	open     func() Reader
	save     Save
	onChange func()
	wake     chan struct{}

	mu    sync.Mutex
	opts  Options
	model string
	last  *sysmon.Sample
	hist  map[string][]float64 // readings by chart, oldest first
}

// New creates a disabled plugin; open makes a reader when the page is
// turned on, save keeps a new order, and onChange is called when the page
// changes.
func New(open func() Reader, save Save, onChange func()) *Plugin {
	if onChange == nil {
		onChange = func() {}
	}
	return &Plugin{open: open, save: save, onChange: onChange, wake: make(chan struct{}, 1)}
}

func (p *Plugin) ID() string { return ID }

func (p *Plugin) SetOptions(o Options) {
	p.mu.Lock()
	toggled := o.Enabled != p.opts.Enabled
	p.opts = o
	if !o.Enabled {
		p.last, p.hist = nil, nil
	}
	p.mu.Unlock()
	if toggled {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
	p.onChange()
}

// Order is the full card order: the ids in order that are cards, then
// the cards it leaves out in their usual order.
func Order(order []string) []string {
	out := make([]string, 0, len(Cards))
	for _, id := range order {
		if slices.Contains(Cards, id) && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	for _, id := range Cards {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// Run reads while the page is on, until ctx ends. The first reading after
// the page is turned on only starts the counts (CPU time, network and
// disk bytes).
func (p *Plugin) Run(ctx context.Context) {
	var r Reader
	t := time.NewTicker(Every)
	defer t.Stop()
	for {
		p.mu.Lock()
		on := p.opts.Enabled
		p.mu.Unlock()
		switch {
		case on && r == nil:
			r = p.open()
			r.Read(ctx)
			t.Reset(Every)
			select { // the wake that turned the page on; read again after Every
			case <-p.wake:
			default:
			}
		case on:
			s := r.Read(ctx)
			p.add(r.Model(), s)
			p.onChange()
		default:
			r = nil
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-p.wake:
		}
	}
}

// add keeps a reading and what its charts need: running totals for the
// stacked ones.
func (p *Plugin) add(cpuModel string, s sysmon.Sample) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.opts.Enabled {
		return // turned off while reading
	}
	if p.hist == nil {
		p.hist = map[string][]float64{}
	}
	push := func(k string, v float64) {
		h := append(p.hist[k], v)
		p.hist[k] = h[max(len(h)-History, 0):]
	}
	push("cpu", s.CPU)
	push("cpu_user", s.CPUUser)
	if g := s.GPU; g.Util >= 0 {
		push("gpu", g.Util)
		if g.Renderer >= 0 && g.Tiler >= 0 {
			push("gpu_renderer", g.Renderer)
			push("gpu_tiler", g.Tiler)
		}
	}
	if m := s.Mem; m.Total > 0 {
		share := func(b uint64) float64 { return float64(b) / float64(m.Total) * 100 }
		push("mem", m.Pct())
		push("mem_app", share(m.App))
		push("mem_app_wired", share(m.App+m.Wired))
	}
	push("fan", fastest(s.Fans).RPM)
	if s.Rx >= 0 {
		push("net", s.Rx)
		push("net_sum", s.Rx+s.Tx)
	}
	if s.DiskRead >= 0 {
		push("disk_read", s.DiskRead)
		push("disk_sum", s.DiskRead+s.DiskWrite)
	}
	p.model, p.last = cpuModel, &s
}

// Page is the dashboard: the cards the computer can fill, in the saved
// order, the first Big of them big.
func (p *Plugin) Page() (model.Page, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.opts.Enabled {
		return model.Page{}, false
	}
	pg := model.Page{ID: ID, Title: "性能", Items: []model.PageItem{}}
	if p.last == nil {
		pg.Summary = "读取中…"
		return pg, true
	}
	s := *p.last
	pg.Summary = summary(s)
	for _, id := range Order(p.opts.Order) {
		if it, ok := p.card(id, s, len(pg.Items) < Big); ok {
			pg.Items = append(pg.Items, it)
		}
	}
	pg.Movable = len(pg.Items) > 1
	return pg, true
}

// Swap trades the places of two cards, one dropped on the other, and
// saves the order.
func (p *Plugin) Swap(ctx context.Context, item, to string) error {
	p.mu.Lock()
	on := p.opts.Enabled
	order := Order(p.opts.Order)
	p.mu.Unlock()
	if !on {
		return plugins.ErrNotFound
	}
	i, j := slices.Index(order, item), slices.Index(order, to)
	if i < 0 || j < 0 {
		return plugins.ErrNotFound
	}
	if i == j {
		return nil
	}
	order[i], order[j] = order[j], order[i]
	if err := p.save(order); err != nil {
		return err
	}
	p.mu.Lock()
	p.opts.Order = order
	p.mu.Unlock()
	p.onChange()
	return nil
}

// Do takes no acts: the cards only show readings.
func (p *Plugin) Do(ctx context.Context, item, act string) error {
	p.mu.Lock()
	on := p.opts.Enabled
	p.mu.Unlock()
	if !on {
		return plugins.ErrNotFound
	}
	return plugins.ErrBadAct
}
