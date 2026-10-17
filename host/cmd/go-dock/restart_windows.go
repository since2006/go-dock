//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/since2006/go-dock/host/internal/selfexe"
)

// restartSelf starts the freshly installed binary and lets this process
// exit; Windows has no exec. The new process runs without a console window.
func restartSelf() error {
	exe, err := selfexe.Path()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), "GO_DOCK_RESTARTED=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW}
	return cmd.Start()
}

// detachConsole closes the console window that Windows opens for a console
// program started at login.
func detachConsole() {
	_, _, _ = windows.NewLazySystemDLL("kernel32.dll").NewProc("FreeConsole").Call()
}
