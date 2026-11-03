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
)

func TestIoregGPU(t *testing.T) {
	out := []byte(`+-o AGXAcceleratorG15X  <class AGXAcceleratorG15X, id 0x1000004a1>
    {
      "model" = "Apple M3 Max"
      "PerformanceStatistics" = {"In use system memory"=812384256,"Tiler Utilization %"=12,"Renderer Utilization %"=30,"Device Utilization %"=37}
    }
+-o AMDRadeonX6000_AMDNavi14GraphicsAccelerator
    {
      "model" = <"AMD Radeon Pro 5500M">
      "PerformanceStatistics" = {"Device Utilization %"=5}
    }`)
	if g := ioregGPU(out); g != 37 {
		t.Fatalf("ioregGPU = %v, want the busiest GPU's 37", g)
	}
	if g := ioregGPU(bytes.ReplaceAll(out, []byte("=37}"), []byte("=1}"))); g != 5 {
		t.Fatalf("ioregGPU = %v, want the AMD GPU's 5", g)
	}
	if g := ioregGPU([]byte(`+-o IOAccelerator`)); g != -1 {
		t.Fatalf("no utilization read as %v", g)
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
	if g := nvidiaSMI([]byte("37\n81\n[N/A]\n")); g != 81 {
		t.Fatalf("nvidiaSMI = %v, want the busiest GPU's 81", g)
	}
	if g := nvidiaSMI([]byte("No devices were found\n")); g != -1 {
		t.Fatalf("no GPU read as %v", g)
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
	// App memory (anonymous less purgeable), wired and compressed.
	if used, ok := vmStat(out); !ok || used != (667632-35910+300368+553045)*16384 {
		t.Fatalf("vmStat = %d %v", used, ok)
	}
	if _, ok := vmStat([]byte("Pages free: 1.\n")); ok {
		t.Fatal("vm_stat without a page size read")
	}
}

func TestCPUBusy(t *testing.T) {
	prev := cpu.TimesStat{User: 100, Nice: 0, System: 50, Idle: 850}
	cur := cpu.TimesStat{User: 130, Nice: 10, System: 70, Idle: 890, Iowait: 0}
	// 100 ticks: 40 user, 20 system, 40 idle.
	if busy := cpuBusy(prev, cur); busy != 60 {
		t.Fatalf("cpuBusy = %v", busy)
	}
	if busy := cpuBusy(cur, cur); busy != 0 {
		t.Fatalf("no time passed: %v", busy)
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
	write("hwmon/hwmon2/fan2_input", "0")
	write("hwmon/hwmon3/fan1_input", "oops")
	if got := hwmonFans(filepath.Join(root, "hwmon")); !reflect.DeepEqual(got, []float64{1200, 0}) {
		t.Fatalf("hwmonFans = %v", got)
	}
	if fans := hwmonFans(filepath.Join(root, "none")); fans != nil {
		t.Fatalf("fans without hwmon: %v", fans)
	}

	write("drm/card0/device/gpu_busy_percent", "12")
	write("drm/card1/device/gpu_busy_percent", "45")
	if g := amdGPU(filepath.Join(root, "drm")); g != 45 {
		t.Fatalf("amdGPU = %v, want 45", g)
	}
	if g := amdGPU(filepath.Join(root, "none")); g != -1 {
		t.Fatalf("GPU without sysfs read as %v", g)
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

func TestUsagePct(t *testing.T) {
	if p := (Usage{Used: 1, Total: 4}).Pct(); p != 25 {
		t.Fatalf("pct = %v", p)
	}
	if p := (Usage{}).Pct(); p != 0 {
		t.Fatalf("pct of nothing = %v", p)
	}
}
