package sysmon

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/sensors"
)

func TestIoregGPU(t *testing.T) {
	out := []byte(`+-o AGXAcceleratorG15X  <class AGXAcceleratorG15X, id 0x1000004a1>
    {
      "model" = "Apple M3 Max"
      "gpu-core-count" = 30
      "PerformanceStatistics" = {"In use system memory"=812384256,"Tiler Utilization %"=12,"Renderer Utilization %"=30,"Device Utilization %"=37}
    }
+-o AMDRadeonX6000_AMDNavi14GraphicsAccelerator
    {
      "model" = <"AMD Radeon Pro 5500M">
      "PerformanceStatistics" = {"Device Utilization %"=5}
    }`)
	want := GPU{Name: "Apple M3 Max", Cores: 30, Util: 37, Renderer: 30, Tiler: 12}
	if g := ioregGPU(out); g != want {
		t.Fatalf("ioregGPU = %+v, want the busiest GPU %+v", g, want)
	}
	amd := bytes.ReplaceAll(out, []byte("=37}"), []byte("=1}"))
	if g := ioregGPU(amd); g.Name != "AMD Radeon Pro 5500M" || g.Util != 5 || g.Renderer != -1 || g.Cores != 0 {
		t.Fatalf("ioregGPU = %+v, want the AMD GPU", g)
	}
	if g := ioregGPU([]byte(`+-o IOAccelerator`)); g != NoGPU {
		t.Fatalf("no utilization read as %+v", g)
	}
}

func TestSMCNumber(t *testing.T) {
	typ := func(s string) uint32 { return binary.BigEndian.Uint32([]byte(s)) }
	flt := binary.LittleEndian.AppendUint32(nil, math.Float32bits(2150.5))
	cases := []struct {
		typ  string
		b    []byte
		want float64
	}{
		{"flt ", flt, 2150.5},              // Apple silicon fans
		{"fpe2", []byte{0x21, 0x98}, 2150}, // Intel Macs: 8600 / 4
		{"ui8 ", []byte{2}, 2},             // FNum
		{"ui16", []byte{0x01, 0x00}, 256},
		{"ui32", []byte{0, 0, 0x01, 0x00}, 256},
	}
	for _, c := range cases {
		if got, err := smcNumber(typ(c.typ), c.b); err != nil || got != c.want {
			t.Errorf("%q = %v, %v; want %v", c.typ, got, err, c.want)
		}
	}
	if _, err := smcNumber(typ("sp78"), []byte{1, 2}); err == nil {
		t.Error("unknown type decoded")
	}
	if _, err := smcNumber(typ("flt "), []byte{1, 2}); err == nil {
		t.Error("short float decoded")
	}
}

func TestNvidiaSMI(t *testing.T) {
	g := nvidiaSMI([]byte("NVIDIA GeForce GTX 1080, 37, 52, 2048, 8192\nNVIDIA GeForce RTX 3080, 81, 64, 6144, 10240\nTesla, [N/A], [N/A], [N/A], [N/A]\n"))
	want := GPU{Name: "NVIDIA GeForce RTX 3080", Util: 81, Renderer: -1, Tiler: -1, Temp: 64, Mem: Usage{Used: 6 << 30, Total: 10 << 30}}
	if g != want {
		t.Fatalf("nvidiaSMI = %+v, want %+v", g, want)
	}
	if g := nvidiaSMI([]byte("No devices were found\n")); g != NoGPU {
		t.Fatalf("no GPU read as %+v", g)
	}
}

func TestVMStat(t *testing.T) {
	out := []byte(`Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                   400355.
Pages active:                                 515050.
Pages inactive:                               524752.
Pages wired down:                             300368.
Pages purgeable:                               35910.
"Translation faults":                      518220794.
File-backed pages:                            382568.
Anonymous pages:                              667632.
Pages stored in compressor:                  1064070.
Pages occupied by compressor:                 553045.
Decompressions:                             21251487.
`)
	app, wired, compressed, ok := vmStat(out)
	if !ok || app != (667632-35910)*16384 || wired != 300368*16384 || compressed != 553045*16384 {
		t.Fatalf("vmStat = %d %d %d %v", app, wired, compressed, ok)
	}
	if _, _, _, ok := vmStat([]byte("Pages free: 1.\n")); ok {
		t.Fatal("vm_stat without a page size read")
	}
}

func TestCPUShares(t *testing.T) {
	prev := cpu.TimesStat{User: 100, Nice: 0, System: 50, Idle: 850}
	cur := cpu.TimesStat{User: 130, Nice: 10, System: 70, Idle: 890, Iowait: 0}
	// 100 ticks: 40 user, 20 system, 40 idle.
	if busy, user := cpuShares(prev, cur); busy != 60 || user != 40 {
		t.Fatalf("cpuShares = %v %v", busy, user)
	}
	if busy, user := cpuShares(cur, cur); busy != 0 || user != 0 {
		t.Fatalf("no time passed: %v %v", busy, user)
	}
}

func TestSysfs(t *testing.T) {
	root := t.TempDir()
	write := func(path, v string) {
		t.Helper()
		p := filepath.Join(root, path)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("hwmon/hwmon2/fan1_input", "1200")
	write("hwmon/hwmon2/fan1_max", "3000")
	write("hwmon/hwmon2/fan2_input", "0")
	write("hwmon/hwmon3/fan1_input", "oops")
	want := []Fan{{RPM: 1200, Max: 3000}, {RPM: 0}}
	if got := hwmonFans(filepath.Join(root, "hwmon")); !reflect.DeepEqual(got, want) {
		t.Fatalf("hwmonFans = %+v, want %+v", got, want)
	}
	if fans := hwmonFans(filepath.Join(root, "none")); fans != nil {
		t.Fatalf("fans without hwmon: %+v", fans)
	}

	write("drm/card0/device/gpu_busy_percent", "12")
	write("drm/card1/device/gpu_busy_percent", "45")
	write("drm/card1/device/mem_info_vram_used", "1073741824")
	write("drm/card1/device/mem_info_vram_total", "8589934592")
	wantGPU := GPU{Util: 45, Renderer: -1, Tiler: -1, Mem: Usage{Used: 1 << 30, Total: 8 << 30}}
	if g := amdGPU(filepath.Join(root, "drm")); g != wantGPU {
		t.Fatalf("amdGPU = %+v, want %+v", g, wantGPU)
	}
	if g := amdGPU(filepath.Join(root, "none")); g != NoGPU {
		t.Fatalf("GPU without sysfs read as %+v", g)
	}
}

func TestPickTemps(t *testing.T) {
	ts := func(kv ...any) []sensors.TemperatureStat {
		var out []sensors.TemperatureStat
		for i := 0; i < len(kv); i += 2 {
			out = append(out, sensors.TemperatureStat{SensorKey: kv[i].(string), Temperature: kv[i+1].(float64)})
		}
		return out
	}
	cases := []struct {
		name       string
		in         []sensors.TemperatureStat
		cpuT, gpuT float64
	}{
		{"M3", ts("PMU tdie1", 48.2, "PMU tdie8", 50.7, "PMU tdev1", 41.9, "NAND CH0 temp", 37.0), 50.7, 0},
		{"M1", ts("pACC MTR Temp Sensor2", 55.0, "PMU tdie1", 60.0, "GPU MTR Temp Sensor1", 44.0), 55, 44},
		{"Intel Linux", ts("coretemp_core_0", 70.0, "coretemp_package_id_0", 66.0, "acpitz", 30.0, "nvme_composite", 40.0), 66, 0},
		{"AMD Linux", ts("k10temp_tctl", 61.5, "amdgpu_edge", 47.0, "acpitz", 30.0), 61.5, 47},
		{"bogus readings", ts("coretemp_package_id_0", 0.0, "acpitz", 200.0, "cpu_thermal", 45.0), 45, 0},
	}
	for _, c := range cases {
		if cpuT, gpuT := pickTemps(c.in); cpuT != c.cpuT || gpuT != c.gpuT {
			t.Errorf("%s: cpu %v gpu %v, want %v %v", c.name, cpuT, gpuT, c.cpuT, c.gpuT)
		}
	}
}

func TestPhysicalNIC(t *testing.T) {
	for name, want := range map[string]bool{
		"en0": true, "eth0": true, "wlp3s0": true, "Wi-Fi": true, "Ethernet": true, "Local Area Connection": true,
		"lo0": false, "lo": false, "utun3": false, "awdl0": false, "bridge100": false, "docker0": false,
		"veth12ab": false, "tailscale0": false, "Loopback Pseudo-Interface 1": false, "vEthernet (WSL)": false,
	} {
		if got := physicalNIC(name); got != want {
			t.Errorf("physicalNIC(%q) = %v", name, got)
		}
	}
}

func TestCleanModel(t *testing.T) {
	for in, want := range map[string]string{
		"Apple M3 Max": "Apple M3 Max",
		"Intel(R) Core(TM) i7-8700 CPU @ 3.20GHz":   "Intel Core i7-8700",
		"AMD Ryzen 9 5900X 12-Core Processor":       "AMD Ryzen 9 5900X",
		"Intel(R) Xeon(R) CPU E5-2680 v4 @ 2.40GHz": "Intel Xeon CPU E5-2680 v4",
		"  ARM   Cortex-A72 ":                       "ARM Cortex-A72",
	} {
		if got := CleanModel(in); got != want {
			t.Errorf("CleanModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWholeDisk(t *testing.T) {
	for name, want := range map[string]bool{
		"disk0": true, "sda": true, "nvme0n1": true, "mmcblk0": true, "C:": true, "vda": true,
		"sda1": false, "nvme0n1p2": false, "mmcblk0p1": false, "loop3": false, "dm-0": false, "zram0": false, "sr0": false,
	} {
		if got := wholeDisk(name); got != want {
			t.Errorf("wholeDisk(%q) = %v", name, got)
		}
	}
}

func TestUsagePct(t *testing.T) {
	if p := (Usage{Used: 1, Total: 4}).Pct(); p != 25 {
		t.Fatalf("pct = %v", p)
	}
	if p := (Usage{}).Pct(); p != 0 {
		t.Fatalf("pct of nothing = %v", p)
	}
}
