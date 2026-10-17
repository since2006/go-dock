package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime/debug"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
)

const (
	appServerTimeout  = 20 * time.Second
	appServerFirstRun = 5 * time.Second
	defaultInterval   = 10 * time.Minute
)

// appServerLoop asks `codex app-server` for rate limits periodically, so
// the gauges stay fresh while Codex itself is idle.
func (p *Provider) appServerLoop(ctx context.Context) {
	wait := appServerFirstRun
	warned := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
			wait = appServerFirstRun
			continue
		case <-time.After(wait):
		}
		opts := p.options()
		wait = opts.AppServerInterval
		if wait <= 0 {
			wait = defaultInterval
		}
		if !opts.AppServerPoll {
			continue
		}
		path, err := exec.LookPath("codex")
		if err != nil {
			if !warned {
				p.log.Debug("未找到 codex 命令，跳过 app-server 额度查询")
				warned = true
			}
			continue
		}
		l, err := p.queryAppServer(ctx, path)
		if err != nil {
			p.log.Warn("app-server 额度查询失败", "err", err)
			continue
		}
		p.log.Debug("app-server 额度已更新")
		p.publishLimits(l)
	}
}

func (p *Provider) queryAppServer(ctx context.Context, path string) (limits, error) {
	ctx, cancel := context.WithTimeout(ctx, appServerTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "app-server")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return limits{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return limits{}, err
	}
	if err := cmd.Start(); err != nil {
		return limits{}, err
	}
	l, err := readRateLimits(stdout, stdin, clientVersion(), time.Now())
	// Closing stdin ends the stdio transport; kill only if it lingers, so
	// npm's node wrapper does not leave the native binary behind.
	stdin.Close()
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		cmd.Process.Kill()
		<-done
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	return l, err
}

func clientVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type appWindow struct {
	UsedPercent        *float64 `json:"usedPercent"`
	WindowDurationMins *int     `json:"windowDurationMins"`
	ResetsAt           *int64   `json:"resetsAt"`
}

type appSnapshot struct {
	Primary   *appWindow `json:"primary"`
	Secondary *appWindow `json:"secondary"`
}

func (w *appWindow) window(def int) *model.Window {
	if w == nil || w.UsedPercent == nil {
		return nil
	}
	out := &model.Window{UsedPct: *w.UsedPercent, WindowMinutes: orDefault(w.WindowDurationMins, def)}
	if w.ResetsAt != nil && *w.ResetsAt > 0 {
		out.ResetsAt = time.Unix(*w.ResetsAt, 0)
	}
	return out
}

// readRateLimits runs the app-server exchange over r/w: initialize,
// initialized, then account/rateLimits/read. Messages carry no "jsonrpc"
// field, as the Codex app-server expects.
func readRateLimits(r io.Reader, w io.Writer, version string, now time.Time) (limits, error) {
	enc := json.NewEncoder(w)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)

	await := func(id string) (json.RawMessage, error) {
		for sc.Scan() {
			var m rpcMessage
			if json.Unmarshal(sc.Bytes(), &m) != nil || string(m.ID) != id {
				continue // notifications and unrelated messages
			}
			if m.Error != nil {
				return nil, fmt.Errorf("app-server 错误 %d: %s", m.Error.Code, m.Error.Message)
			}
			return m.Result, nil
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("app-server 提前退出")
	}

	if err := enc.Encode(map[string]any{
		"method": "initialize",
		"id":     0,
		"params": map[string]any{"clientInfo": map[string]any{"name": "go-dock", "title": "go-dock", "version": version}},
	}); err != nil {
		return limits{}, err
	}
	if _, err := await("0"); err != nil {
		return limits{}, err
	}
	if err := enc.Encode(map[string]any{"method": "initialized"}); err != nil {
		return limits{}, err
	}
	if err := enc.Encode(map[string]any{"method": "account/rateLimits/read", "id": 1}); err != nil {
		return limits{}, err
	}
	raw, err := await("1")
	if err != nil {
		return limits{}, err
	}

	var res struct {
		RateLimits *appSnapshot            `json:"rateLimits"`
		ByLimitID  map[string]*appSnapshot `json:"rateLimitsByLimitId"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return limits{}, fmt.Errorf("解析额度: %w", err)
	}
	snap := res.ByLimitID["codex"]
	if snap == nil {
		snap = res.RateLimits
	}
	if snap == nil {
		return limits{}, errors.New("app-server 未返回额度（可能未使用 ChatGPT 登录）")
	}
	l := limits{five: snap.Primary.window(300), seven: snap.Secondary.window(10080), at: now, source: "app-server"}
	if l.five == nil && l.seven == nil {
		return limits{}, errors.New("app-server 返回的额度为空")
	}
	return l, nil
}
