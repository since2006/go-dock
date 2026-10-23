package sysmon

import (
	"context"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// Windows has no standard way to read fans; NVIDIA cards report through
// nvidia-smi, which their driver installs.
type win struct {
	smi string
}

func newPlatform() platform {
	p := &win{}
	p.smi, _ = exec.LookPath("nvidia-smi")
	return p
}

func (*win) fans(context.Context) []Fan { return nil }

// memory: gopsutil's Used is what Task Manager shows as 正在使用.
func (*win) memory(context.Context, *Memory) {}

func (w *win) gpu(ctx context.Context) GPU {
	if w.smi == "" {
		return NoGPU
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, w.smi, smiArgs...)
	// go-dock runs without a console; do not flash one every two seconds.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.Output()
	if err != nil {
		return NoGPU
	}
	return nvidiaSMI(out)
}
