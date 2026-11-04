// Package perf adds the computer's CPU, GPU, memory, fans, the chip's
// temperature and disk to the Agent page's line, read every two seconds
// while it is on. What the computer cannot report (a GPU's load on some,
// fans and the temperature on Windows) is left out.
package perf

import (
	"context"
	"sync"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/sysmon"
)

// Every is how often it reads.
const Every = 2 * time.Second

// Reader takes samples; *sysmon.Collector implements it.
type Reader interface {
	Read(ctx context.Context) sysmon.Sample
}

type Options struct {
	Enabled bool
}

type Plugin struct {
	open     func() Reader
	onChange func()
	wake     chan struct{}

	mu   sync.Mutex
	opts Options
	last *sysmon.Sample
}

// New creates a disabled plugin; open makes a reader when it is turned
// on, and onChange is called when its readings change.
func New(open func() Reader, onChange func()) *Plugin {
	if onChange == nil {
		onChange = func() {}
	}
	return &Plugin{open: open, onChange: onChange, wake: make(chan struct{}, 1)}
}

func (p *Plugin) SetOptions(o Options) {
	p.mu.Lock()
	toggled := o.Enabled != p.opts.Enabled
	p.opts = o
	if !o.Enabled {
		p.last = nil
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

// Run reads while it is on, until ctx ends. The first reading after it is
// turned on only starts the count of CPU time.
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
			select { // the wake that turned it on; read again after Every
			case <-p.wake:
			default:
			}
		case on:
			p.add(r.Read(ctx))
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

// add keeps the latest sample.
func (p *Plugin) add(s sysmon.Sample) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.opts.Enabled { // else turned off while reading
		p.last = &s
	}
}

// Readings are the latest sample's, none while off or before the first.
func (p *Plugin) Readings() []model.Reading {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.opts.Enabled || p.last == nil {
		return nil
	}
	return readings(*p.last)
}
