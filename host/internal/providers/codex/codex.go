// Package codex collects OpenAI Codex CLI session state, plan usage and
// token statistics.
package codex

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/providers"
)

const P = model.ProviderCodex

// Home returns the Codex home dir.
func Home(override string) string {
	return providers.ResolveHome(override, "CODEX_HOME", ".codex")
}

type Options struct {
	// AppServerPoll periodically asks `codex app-server` for rate limits.
	AppServerPoll     bool
	AppServerInterval time.Duration
}

// Provider polls Codex's rollout files and accepts hook payloads forwarded
// by the web server.
type Provider struct {
	home string
	sink providers.Sink
	log  *slog.Logger

	rollouts *rollouts

	mu   sync.Mutex
	opts Options
	wake chan struct{} // nudges the app-server loop after SetOptions
}

func New(home string, sink providers.Sink, opts Options, log *slog.Logger) *Provider {
	return &Provider{
		home:     home,
		sink:     sink,
		log:      log.With("provider", "codex"),
		rollouts: newRollouts(home, time.Local),
		opts:     opts,
		wake:     make(chan struct{}, 1),
	}
}

func (p *Provider) Home() string { return p.home }

func (p *Provider) SetOptions(o Options) {
	p.mu.Lock()
	p.opts = o
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Provider) options() Options {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.opts
}

// Run polls until ctx ends.
func (p *Provider) Run(ctx context.Context) {
	p.log.Info("采集启动", "home", p.home)
	p.poll(true)
	go p.appServerLoop(ctx)

	fast := time.NewTicker(2 * time.Second)
	defer fast.Stop()
	scan := time.NewTicker(30 * time.Second)
	defer scan.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-fast.C:
			p.poll(false)
		case <-scan.C:
			p.poll(true)
		}
	}
}

func (p *Provider) poll(rescan bool) {
	now := time.Now()
	res := p.rollouts.poll(rescan, now)
	for _, o := range res.obs {
		p.sink.Observe(o)
	}
	if res.limits != nil {
		p.publishLimits(*res.limits)
	}
	// A rescan also republishes so totals roll over at midnight.
	if res.tokensChanged || rescan {
		today, week := p.rollouts.totals(now)
		p.sink.UpdateUsage(P, func(u *model.Usage) {
			u.Today = today
			u.Week = week
		})
	}
}

// limits is one rate-limit reading and when it was taken.
type limits struct {
	five, seven *model.Window
	at          time.Time
	source      string
}

// publishLimits applies l unless the hub already holds a newer reading,
// e.g. app-server data fresher than the last rollout line.
func (p *Provider) publishLimits(l limits) {
	if l.five == nil && l.seven == nil {
		return
	}
	p.sink.UpdateUsage(P, func(u *model.Usage) {
		if l.at.Before(u.LimitsUpdated) {
			return
		}
		if l.five != nil {
			u.FiveHour = l.five
		}
		if l.seven != nil {
			u.SevenDay = l.seven
		}
		u.LimitsUpdated = l.at
		u.LimitsSource = l.source
	})
}
