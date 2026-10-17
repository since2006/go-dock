//go:build !windows

package main

import (
	"os"
	"syscall"

	"github.com/since2006/go-dock/host/internal/selfexe"
)

// restartSelf replaces this process with the freshly installed binary. The
// PID stays the same, so a launchd or systemd parent keeps tracking it.
func restartSelf() error {
	exe, err := selfexe.Path()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, append(os.Environ(), "GO_DOCK_RESTARTED=1"))
}

func detachConsole() {}
