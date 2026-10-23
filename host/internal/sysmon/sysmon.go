// Package sysmon reads the computer's load for the 性能 page: CPU, GPU,
// memory, fans, network and disk. Each platform reads what it can; what it
// cannot is left out of the sample.
package sysmon

import (
	"context"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/sensors"
)

// Sample is one reading.
type Sample struct {
	CPU     float64 // busy percent
	CPUUser float64 // of which user programs; the rest is the system's
	CPUTemp float64 // °C; 0 when unknown
	GPU     GPU
	Mem     Memory
	Disk    Usage
	Fans    []Fan
	// Network bytes per second over all physical interfaces; -1 until a
	// second reading.
	Rx, Tx float64
	// RxTotal and TxTotal are the bytes those interfaces moved since boot.
	RxTotal, TxTotal uint64
	// Disk bytes per second read and written over all disks; -1 until a
	// second reading.
	DiskRead, DiskWrite float64
	Uptime              time.Duration // 0 when unknown
}

// Memory is the RAM as the system's own monitor counts it: on macOS what
// Activity Monitor calls 内存已使用 (app memory, wired and compressed),
// elsewhere what programs hold beyond what the system can take back.
type Memory struct {
	Usage
	// macOS: what Used is made of: app memory, wired and what the
	// compressor occupies; all 0 elsewhere.
	App, Wired, Compressed uint64
	Swap                   Usage
	// Pressure is macOS's memory pressure: 1 normal, 2 warn, 4 critical;
	// 0 unknown.
	Pressure int
}

// GPU is the busiest graphics processor. A percent below 0 is unknown.
type GPU struct {
	Name     string
	Cores    int
	Util     float64 // busy percent; -1 when the load cannot be read
	Renderer float64 // Apple GPUs: the 3D pipeline's share; -1 elsewhere
	Tiler    float64 // Apple GPUs: geometry processing; -1 elsewhere
	Temp     float64 // °C; 0 when unknown
	Mem      Usage   // video memory, where the driver tells
}

// NoGPU is a GPU whose load is unknown.
var NoGPU = GPU{Util: -1, Renderer: -1, Tiler: -1}

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

// Fan is one fan's speed in rpm; Max is 0 when unknown.
type Fan struct {
	RPM, Max float64
}

// platform reads what gopsutil does not: fans and the GPU.
type platform interface {
	fans(ctx context.Context) []Fan
	gpu(ctx context.Context) GPU
	// memory corrects what gopsutil read where it counts differently from
	// the system's own monitor.
	memory(ctx context.Context, m *Memory)
}

// Collector takes samples. It keeps the last network counters, so use
// one per reader and call Read at a steady pace.
type Collector struct {
	plat  platform
	model string
	disk  string

	cpu    cpu.TimesStat // the last CPU times
	netAt  time.Time
	rx, tx uint64
	diskAt time.Time
	dr, dw uint64
}

func New() *Collector {
	c := &Collector{plat: newPlatform(), disk: diskPath()}
	if info, err := cpu.Info(); err == nil && len(info) > 0 {
		c.model = CleanModel(info[0].ModelName)
	}
	return c
}

// Model names the CPU, e.g. "Apple M3 Max"; "" when unknown.
func (c *Collector) Model() string { return c.model }

// Read takes a sample. It may take a little while (sensors, ioreg,
// nvidia-smi), so call it off any path that must not block.
func (c *Collector) Read(ctx context.Context) Sample {
	s := Sample{GPU: NoGPU, Rx: -1, Tx: -1, DiskRead: -1, DiskWrite: -1}
	if ts, err := cpu.TimesWithContext(ctx, false); err == nil && len(ts) > 0 {
		s.CPU, s.CPUUser = cpuShares(c.cpu, ts[0])
		c.cpu = ts[0]
	}
	if v, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		s.Mem.Usage = Usage{Used: v.Used, Total: v.Total}
	}
	if sw, err := mem.SwapMemoryWithContext(ctx); err == nil {
		s.Mem.Swap = Usage{Used: sw.Used, Total: sw.Total}
	}
	if d, err := disk.UsageWithContext(ctx, c.disk); err == nil {
		s.Disk = Usage{Used: d.Used, Total: d.Total}
	}
	s.GPU = c.plat.gpu(ctx)
	if ts, err := sensors.TemperaturesWithContext(ctx); len(ts) > 0 {
		_ = err // partial lists come with an error
		var gpuT float64
		s.CPUTemp, gpuT = pickTemps(ts)
		if s.GPU.Temp == 0 {
			s.GPU.Temp = gpuT
		}
	}
	if up, err := host.UptimeWithContext(ctx); err == nil {
		s.Uptime = time.Duration(up) * time.Second
	}
	c.plat.memory(ctx, &s.Mem)
	s.Fans = c.plat.fans(ctx)
	if nics, err := net.IOCountersWithContext(ctx, true); err == nil {
		var rx, tx uint64
		for _, n := range nics {
			if physicalNIC(n.Name) {
				rx += n.BytesRecv
				tx += n.BytesSent
			}
		}
		now := time.Now()
		if secs := now.Sub(c.netAt).Seconds(); !c.netAt.IsZero() && secs > 0 && rx >= c.rx && tx >= c.tx {
			s.Rx, s.Tx = float64(rx-c.rx)/secs, float64(tx-c.tx)/secs
		}
		c.netAt, c.rx, c.tx = now, rx, tx
		s.RxTotal, s.TxTotal = rx, tx
	}
	if disks, err := disk.IOCountersWithContext(ctx); err == nil {
		var dr, dw uint64
		for name, d := range disks {
			if wholeDisk(name) {
				dr += d.ReadBytes
				dw += d.WriteBytes
			}
		}
		now := time.Now()
		if secs := now.Sub(c.diskAt).Seconds(); !c.diskAt.IsZero() && secs > 0 && dr >= c.dr && dw >= c.dw {
			s.DiskRead, s.DiskWrite = float64(dr-c.dr)/secs, float64(dw-c.dw)/secs
		}
		c.diskAt, c.dr, c.dw = now, dr, dw
	}
	return s
}

var (
	// Linux lists partitions and virtual devices next to the disks.
	partition   = regexp.MustCompile(`^((sd|hd|vd|xvd)[a-z]+\d+|(nvme\d+n\d+|mmcblk\d+)p\d+)$`)
	virtualDisk = regexp.MustCompile(`^(loop|ram|zram|dm-|sr|fd|md)`)
)

// wholeDisk tells disks from their partitions, so nothing counts twice.
func wholeDisk(name string) bool {
	return !partition.MatchString(name) && !virtualDisk.MatchString(name)
}

// cpuShares turns two readings of the CPU times into the busy percent and
// the user programs' part of it; the first reading (no prev) counts since
// boot.
func cpuShares(prev, cur cpu.TimesStat) (busy, user float64) {
	sum := func(t cpu.TimesStat) (total, idle, user float64) {
		idle = t.Idle + t.Iowait
		user = t.User + t.Nice
		return idle + user + t.System + t.Irq + t.Softirq + t.Steal, idle, user
	}
	t0, i0, u0 := sum(prev)
	t1, i1, u1 := sum(cur)
	dt := t1 - t0
	if dt <= 0 {
		return 0, 0
	}
	busy = min(max((dt-(i1-i0))/dt*100, 0), 100)
	user = min(max((u1-u0)/dt*100, 0), busy)
	return busy, user
}

// diskPath is the volume whose space the page shows: the system's.
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

// Sensor names, most specific first: the CPU's temperature is the hottest
// reading of the first group that has one.
var cpuTempKeys = [][]string{
	{"pacc", "eacc"},                    // Apple silicon CPU clusters (M1, M2)
	{"tdie"},                            // Apple silicon die (M3 and later)
	{"coretemp_package", "cpu_package"}, // Intel
	{"k10temp_tctl", "k10temp_tdie", "zenpower"},
	{"coretemp", "cpu"},
	{"acpitz", "thermal"},
}

// pickTemps finds the CPU's and the GPU's temperatures among the sensors.
func pickTemps(ts []sensors.TemperatureStat) (cpuT, gpuT float64) {
	hottest := func(match func(key string) bool) float64 {
		t := 0.0
		for _, s := range ts {
			if s.Temperature > 0 && s.Temperature < 150 && match(strings.ToLower(s.SensorKey)) {
				t = max(t, s.Temperature)
			}
		}
		return t
	}
	for _, group := range cpuTempKeys {
		cpuT = hottest(func(k string) bool {
			for _, g := range group {
				if strings.Contains(k, g) {
					return !strings.Contains(k, "gpu")
				}
			}
			return false
		})
		if cpuT > 0 {
			break
		}
	}
	gpuT = hottest(func(k string) bool {
		return strings.Contains(k, "gpu") || strings.HasPrefix(k, "nouveau")
	})
	return cpuT, gpuT
}

// Interfaces that carry no traffic of their own: loopback, tunnels, VPNs,
// bridges and virtual machines, whose traffic also crosses a physical one.
var (
	loopback   = regexp.MustCompile(`^lo\d*$`)
	virtualNIC = []string{
		"utun", "awdl", "llw", "bridge", "gif", "stf", "anpi", "ap", "vmnet", "vnic",
		"veth", "docker", "br-", "virbr", "tun", "tap", "wg", "tailscale", "zt",
		"loopback", "vethernet", "isatap", "teredo",
	}
)

func physicalNIC(name string) bool {
	n := strings.ToLower(name)
	if loopback.MatchString(n) {
		return false
	}
	for _, p := range virtualNIC {
		if strings.HasPrefix(n, p) {
			return false
		}
	}
	return true
}

var (
	modelNoise = regexp.MustCompile(`\((R|TM|tm)\)|\s+@.*$|\s+\d+-Core Processor$|\s+(CPU|Processor)$`)
	spaces     = regexp.MustCompile(`\s+`)
)

// CleanModel shortens a CPU name for the page: "Intel(R) Core(TM) i7-8700
// CPU @ 3.20GHz" becomes "Intel Core i7-8700".
func CleanModel(s string) string {
	for {
		t := modelNoise.ReplaceAllString(s, "")
		if t == s {
			break
		}
		s = t
	}
	return strings.TrimSpace(spaces.ReplaceAllString(s, " "))
}
