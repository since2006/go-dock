package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/since2006/go-dock/host/internal/config"
	"github.com/since2006/go-dock/host/internal/ingest"
	"github.com/since2006/go-dock/host/internal/providers/claude"
)

// statusline forwards the Claude Code status line payload to the daemon,
// then runs the user's original status line command with the same input so
// the status line looks exactly as before.
func statusline(args []string) int {
	fs := flag.NewFlagSet("statusline", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", config.DefaultListen, "")
	chainB64 := fs.String("chain", "", "")
	_ = fs.Parse(args)

	body, _ := io.ReadAll(io.LimitReader(os.Stdin, ingest.MaxBody))
	posted := make(chan struct{})
	go func() {
		defer close(posted)
		if len(body) > 0 {
			_ = ingest.Post(context.Background(), *addr, "claude", "statusline", body, 500*time.Millisecond)
		}
	}()
	defer func() {
		select {
		case <-posted:
		case <-time.After(600 * time.Millisecond):
		}
	}()

	chain := ""
	if *chainB64 != "" {
		chain, _ = claude.ChainFromCommand("--chain " + *chainB64)
	}
	if chain == "" {
		fmt.Println(fallbackLine(body))
		return 0
	}

	// Claude Code cancels a status line run when a newer update arrives;
	// pass that on to the wrapped command.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	name, argv := chainShell(chain)
	cmd := exec.CommandContext(ctx, name, argv...)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "go-dock statusline:", err)
		return 1
	}
	return 0
}

// chainShell picks the shell Claude Code itself would use for a status
// line command: sh on Unix; Git Bash, else PowerShell, on Windows.
func chainShell(command string) (string, []string) {
	if runtime.GOOS != "windows" {
		return "/bin/sh", []string{"-c", command}
	}
	if bash := gitBash(); bash != "" {
		return bash, []string{"-c", command}
	}
	return "powershell", []string{"-NoProfile", "-Command", command}
}

func gitBash() string {
	if p := os.Getenv("CLAUDE_CODE_GIT_BASH_PATH"); p != "" {
		return p
	}
	var candidates []string
	if git, err := exec.LookPath("git"); err == nil {
		// <Git>\cmd\git.exe -> <Git>\bin\bash.exe
		candidates = append(candidates, filepath.Join(filepath.Dir(filepath.Dir(git)), "bin", "bash.exe"))
	}
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
		if d := os.Getenv(env); d != "" {
			candidates = append(candidates, filepath.Join(d, "Git", "bin", "bash.exe"))
		}
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}

// fallbackLine is shown when there was no status line to wrap.
func fallbackLine(body []byte) string {
	var p struct {
		Model struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
		RateLimits struct {
			FiveHour *struct {
				Pct float64 `json:"used_percentage"`
			} `json:"five_hour"`
			SevenDay *struct {
				Pct float64 `json:"used_percentage"`
			} `json:"seven_day"`
		} `json:"rate_limits"`
	}
	_ = json.Unmarshal(body, &p)
	parts := []string{}
	if p.Model.DisplayName != "" {
		parts = append(parts, p.Model.DisplayName)
	}
	if w := p.RateLimits.FiveHour; w != nil {
		parts = append(parts, fmt.Sprintf("5h %.0f%%", w.Pct))
	}
	if w := p.RateLimits.SevenDay; w != nil {
		parts = append(parts, fmt.Sprintf("7d %.0f%%", w.Pct))
	}
	if len(parts) == 0 {
		return "go-dock"
	}
	return strings.Join(parts, " · ")
}
