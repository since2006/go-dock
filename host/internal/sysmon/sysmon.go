// Package sysmon reads the computer's load for the line of readings: CPU,
// GPU, memory, the chip's temperature, fans and disk. Each platform reads
// what it can; what it cannot is left out of the sample.
package sysmon

import (
	"context"
	"os"
	"runtime"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
)

// Sample is one reading.
type Sample struct {
	CPU  float64 // busy percent
	GPU  float64 // the busiest GPU's busy percent; -1 when it cannot be read
	Mem  Memory
	Temp float64   // the chip's temperature in °C; 0 when it cannot be read
	Disk Usage     // the system volume
	Fans []float64 // each fan's speed in rpm
}

// Memory is the RAM as the system's own monitor counts it: on macOS what
// Activity Monitor calls 内存已使用 (app memory, wired and compressed),
// elsewhere what programs hold beyond what the system can take back.
type Memory struct {
	Usage
	// Pressure is macOS's memory pressure: 1 normal, 2 warn, 4 critical;
	// 0 unknown.
	Pressure int
}

// Usage is an amount in bytes; Total 0 means unknown.
type Usage struct {
	Used, Total uint64
}

func (u Usage) Pct() float64 {
	if u.Total == 0 {
		return 0
	}
	return float64(u.Used) / float64(u.Total) * 100
}

// platform reads what gopsutil does not, or not everywhere: fans, the GPU
// and the chip's temperature.
type platform interface {
	fans(ctx context.Context) []float64
	gpu(ctx context.Context) float64  // -1 when unknown
	temp(ctx context.Context) float64 // 0 when unknown
	// memory corrects what gopsutil read where it counts differently from
	// the system's own monitor.
	memory(ctx context.Context, m *Memory)
}

// Collector takes samples. It keeps the last CPU times, so use one per
// reader and call Read at a steady pace.
type Collector struct {
	plat platform
	disk string
	cpu  cpu.TimesStat // the last CPU times
}

func New() *Collector {
	return &Collector{plat: newPlatform(), disk: diskPath()}
}

// Read takes a sample. It may take a little while (ioreg, vm_stat,
// nvidia-smi), so call it off any path that must not block.
func (c *Collector) Read(ctx context.Context) Sample {
	s := Sample{GPU: -1}
	if ts, err := cpu.TimesWithContext(ctx, false); err == nil && len(ts) > 0 {
		s.CPU = cpuBusy(c.cpu, ts[0])
		c.cpu = ts[0]
	}
	if v, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		s.Mem.Usage = Usage{Used: v.Used, Total: v.Total}
	}
	if d, err := disk.UsageWithContext(ctx, c.disk); err == nil {
		s.Disk = Usage{Used: d.Used, Total: d.Total}
	}
	s.GPU = c.plat.gpu(ctx)
	c.plat.memory(ctx, &s.Mem)
	s.Temp = c.plat.temp(ctx)
	s.Fans = c.plat.fans(ctx)
	return s
}

// cpuBusy turns two readings of the CPU times into the busy percent; the
// first reading (no prev) counts since boot.
func cpuBusy(prev, cur cpu.TimesStat) float64 {
	sum := func(t cpu.TimesStat) (total, idle float64) {
		idle = t.Idle + t.Iowait
		return idle + t.User + t.Nice + t.System + t.Irq + t.Softirq + t.Steal, idle
	}
	t0, i0 := sum(prev)
	t1, i1 := sum(cur)
	dt := t1 - t0
	if dt <= 0 {
		return 0
	}
	return min(max((dt-(i1-i0))/dt*100, 0), 100)
}

// diskPath is the volume whose space the line shows: the system's.
func diskPath() string {
	switch runtime.GOOS {
	case "darwin":
		// The writable half of the APFS system volume holds the user's data.
		if _, err := os.Stat("/System/Volumes/Data"); err == nil {
			return "/System/Volumes/Data"
		}
	case "windows":
		if d := os.Getenv("SystemDrive"); d != "" {
			return d + `\`
		}
		return `C:\`
	}
	return "/"
}
