package sysmon

import (
	"context"
	"os/exec"
	"time"

	"github.com/shirou/gopsutil/v4/sensors"
	"golang.org/x/sys/unix"
)

type darwin struct {
	smc    *smc
	failed time.Time // the SMC could not be opened; try again later
}

func newPlatform() platform { return &darwin{} }

func (d *darwin) fans(context.Context) []float64 {
	if d.smc == nil {
		if time.Since(d.failed) < time.Minute {
			return nil
		}
		s, err := openSMC()
		if err != nil {
			d.failed = time.Now()
			return nil
		}
		d.smc = s
	}
	fans, err := d.smc.fans()
	if err != nil {
		d.smc.close()
		d.smc, d.failed = nil, time.Now()
		return nil
	}
	return fans
}

// memory counts as Activity Monitor does. gopsutil takes everything but
// free and inactive pages as used, so a Mac that compresses and swaps
// still looks three quarters full; vm_stat tells app memory, wired and
// compressed apart.
func (d *darwin) memory(ctx context.Context, m *Memory) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "/usr/bin/vm_stat").Output(); err == nil {
		if used, ok := vmStat(out); ok {
			m.Used = min(used, m.Total)
		}
	}
	if level, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level"); err == nil {
		m.Pressure = int(level)
	}
}

// temp reads the chip's sensors: on Apple silicon through the HID event
// system, on Intel Macs from the SMC.
func (*darwin) temp(ctx context.Context) float64 {
	ts, _ := sensors.TemperaturesWithContext(ctx) // a partial list may come with an error
	return chipTemp(ts)
}

// gpu reads the GPU's load from the IORegistry, where every GPU driver
// (Apple silicon, AMD, Intel) publishes it.
func (d *darwin) gpu(ctx context.Context) float64 {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-r", "-d", "1", "-c", "IOAccelerator").Output()
	if err != nil {
		return -1
	}
	return ioregGPU(out)
}
