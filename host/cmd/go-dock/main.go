// Command go-dock collects Claude Code and Codex status and shows it on a
// USB AMOLED screen and a local web console.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/since2006/go-dock/host/internal/app"
	"github.com/since2006/go-dock/host/internal/config"
	"github.com/since2006/go-dock/host/internal/ingest"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `go-dock — 在 USB 屏幕和网页上显示 Claude Code / Codex 的用量与任务状态

用法:
  go-dock [run] [--no-browser]      启动后台服务和网页管理台（默认）
  go-dock hook <claude|codex>       由 hooks 调用：把 stdin 中的事件转发给后台服务
  go-dock statusline [--chain ...]  由 Claude Code 状态栏调用：转发额度并运行原状态栏命令
  go-dock version                   显示版本

环境变量:
  GO_DOCK_HOME   配置与日志目录（默认为系统配置目录下的 go-dock）
`

func main() {
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "run":
		os.Exit(run(args))
	case "hook":
		os.Exit(hook(args))
	case "statusline":
		os.Exit(statusline(args))
	case "version":
		fmt.Println(version)
	case "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

func run(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	noBrowser := fs.Bool("no-browser", false, "启动时不打开浏览器")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx, app.Options{Version: version, OpenBrowser: !*noBrowser, Console: os.Stderr}); err != nil {
		fmt.Fprintln(os.Stderr, "go-dock:", err)
		return 1
	}
	return 0
}

// hook forwards a hook payload. It must stay silent and exit 0 whatever
// happens: output or a failure could be read as a decision by the caller.
func hook(args []string) int {
	if len(args) == 0 {
		return 0
	}
	provider := args[0]
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", config.DefaultListen, "")
	if fs.Parse(args[1:]) != nil {
		return 0
	}
	body, err := io.ReadAll(io.LimitReader(os.Stdin, ingest.MaxBody))
	if err != nil || len(body) == 0 {
		return 0
	}
	_ = ingest.Post(context.Background(), *addr, provider, "hook", body, time.Second)
	return 0
}
