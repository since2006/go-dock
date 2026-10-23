// Package claude collects Claude Code session state, plan usage and token
// statistics.
package claude

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/providers"
	"github.com/since2006/go-dock/host/internal/providers/pricing"
)

const P = model.ProviderClaude

// Home returns the Claude Code config dir.
func Home(override string) string {
	return providers.ResolveHome(override, "CLAUDE_CONFIG_DIR", ".claude")
}

// Provider polls Claude Code's local files and accepts hook and statusline
// payloads forwarded by the web server.
type Provider struct {
	home string
	sink providers.Sink
	log  *slog.Logger

	sessions    *sessionsWatcher
	transcripts *transcripts
	limits      limitTracker

	mu     sync.Mutex
	titles map[string]string     // sessionID -> title from transcripts
	turns  map[string]model.Turn // sessionID -> current turn from transcripts
}

func New(home string, sink providers.Sink, prices *pricing.Table, log *slog.Logger) *Provider {
	p := &Provider{
		home:   home,
		sink:   sink,
		log:    log.With("provider", "claude"),
		titles: map[string]string{},
		turns:  map[string]model.Turn{},
	}
	p.sessions = newSessionsWatcher(filepath.Join(home, "sessions"))
	p.transcripts = newTranscripts(filepath.Join(home, "projects"), prices)
	p.transcripts.live = p.sessions.live
	return p
}

func (p *Provider) Home() string { return p.home }

// SetPrices swaps the price table used for cost estimates.
func (p *Provider) SetPrices(t *pricing.Table) {
	p.transcripts.setPrices(t)
	p.publishTokens()
}

// Run polls until ctx ends.
func (p *Provider) Run(ctx context.Context) {
	p.log.Info("采集启动", "home", p.home)
	p.pollTranscripts(true)
	p.pollSessions()

	fast := time.NewTicker(time.Second)
	defer fast.Stop()
	scan := time.NewTicker(30 * time.Second)
	defer scan.Stop()
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-fast.C:
			p.pollSessions()
			if n++; n%2 == 0 {
				p.pollTranscripts(false)
			}
		case <-scan.C:
			p.pollTranscripts(true)
		}
	}
}

func (p *Provider) pollSessions() {
	for _, o := range p.sessions.poll(time.Now()) {
		if !o.End {
			p.fromTranscript(&o)
		}
		p.sink.Observe(o)
	}
}

// fromTranscript adds what the transcripts know about the session, which
// the hub ignored if it arrived before the session itself.
func (p *Provider) fromTranscript(o *model.Observation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if o.Title == "" {
		o.Title = p.titles[o.SessionID]
	}
	if o.NewTurn {
		p.turns[o.SessionID] = model.Turn{} // the previous turn is over
	}
	if turn, ok := p.turns[o.SessionID]; ok && o.Turn == nil {
		o.Turn = &turn
	}
}

func (p *Provider) pollTranscripts(rescan bool) {
	res := p.transcripts.poll(rescan, time.Now())
	for id, title := range res.titles {
		p.mu.Lock()
		changed := p.titles[id] != title
		p.titles[id] = title
		p.mu.Unlock()
		if changed {
			p.sink.Observe(model.Observation{Provider: P, SessionID: id, Title: title})
		}
	}
	for id, turn := range res.turns {
		p.mu.Lock()
		old, known := p.turns[id]
		p.turns[id] = turn
		p.mu.Unlock()
		if !known || old != turn {
			p.sink.Observe(model.Observation{Provider: P, SessionID: id, Turn: &turn})
		}
	}
	// A rescan also republishes so totals roll over at midnight.
	if res.changed || rescan {
		p.publishTokens()
	}
	for _, o := range res.agents {
		p.sink.Observe(o)
	}
}

func (p *Provider) publishTokens() {
	today, week := p.transcripts.totals(time.Now())
	p.sink.UpdateUsage(P, func(u *model.Usage) {
		u.Today = today
		u.Week = week
	})
}
