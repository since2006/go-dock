package sysmon

import (
	"context"
	"os/exec"
	"time"
)

type linux struct {
	smi string // nvidia-smi, if installed
}

func newPlatform() platform {
	p := &linux{}
	p.smi, _ = exec.LookPath("nvidia-smi")
	return p
}

func (*linux) fans(context.Context) []Fan { return hwmonFans("/sys/class/hwmon") }

// memory: gopsutil's Used is MemTotal - MemAvailable, as free(1) counts.
func (*linux) memory(context.Context, *Memory) {}

// gpu asks nvidia-smi for NVIDIA cards and reads sysfs for AMD ones.
func (l *linux) gpu(ctx context.Context) GPU {
	if l.smi != "" {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, l.smi, smiArgs...).Output(); err == nil {
			if g := nvidiaSMI(out); g.Util >= 0 {
				return g
			}
		}
	}
	return amdGPU("/sys/class/drm")
}
