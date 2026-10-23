package sysmon

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The parsers below serve one platform each but are plain functions, so
// they are tested everywhere.

var (
	ioregUtil     = regexp.MustCompile(`"Device Utilization %"\s*=\s*(\d+)`)
	ioregRenderer = regexp.MustCompile(`"Renderer Utilization %"\s*=\s*(\d+)`)
	ioregTiler    = regexp.MustCompile(`"Tiler Utilization %"\s*=\s*(\d+)`)
	ioregModel    = regexp.MustCompile(`"model"\s*=\s*<?"([^"]*)"`)
	ioregCores    = regexp.MustCompile(`"gpu-core-count"\s*=\s*(\d+)`)
)

// ioregGPU reads the busiest GPU from the output of `ioreg -r -d 1 -c
// IOAccelerator` (macOS), one "+-o" entry per GPU: its load, and on Apple
// silicon the renderer's and tiler's and the core count.
func ioregGPU(out []byte) GPU {
	best := NoGPU
	for _, entry := range bytes.Split(out, []byte("+-o ")) {
		util, ok := ioregNumber(ioregUtil, entry)
		if !ok || util <= best.Util {
			continue
		}
		g := NoGPU
		g.Util = min(util, 100)
		if v, ok := ioregNumber(ioregRenderer, entry); ok {
			g.Renderer = min(v, 100)
		}
		if v, ok := ioregNumber(ioregTiler, entry); ok {
			g.Tiler = min(v, 100)
		}
		if m := ioregModel.FindSubmatch(entry); m != nil {
			g.Name = string(m[1])
		}
		if v, ok := ioregNumber(ioregCores, entry); ok {
			g.Cores = int(v)
		}
		best = g
	}
	return best
}

func ioregNumber(re *regexp.Regexp, b []byte) (float64, bool) {
	m := re.FindSubmatch(b)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(string(m[1]), 64)
	return v, err == nil
}

var (
	vmPageSize = regexp.MustCompile(`page size of (\d+) bytes`)
	vmLine     = regexp.MustCompile(`(?m)^"?([^":]+)"?:\s+(\d+)\.`)
)

// vmStat reads vm_stat's output (macOS) as Activity Monitor counts memory:
// app memory (anonymous pages that cannot be purged), wired and what the
// compressor occupies, which add up to 内存已使用.
func vmStat(out []byte) (app, wired, compressed uint64, ok bool) {
	m := vmPageSize.FindSubmatch(out)
	if m == nil {
		return 0, 0, 0, false
	}
	page, _ := strconv.ParseUint(string(m[1]), 10, 64)
	pages := map[string]uint64{}
	for _, l := range vmLine.FindAllSubmatch(out, -1) {
		pages[string(l[1])], _ = strconv.ParseUint(string(l[2]), 10, 64)
	}
	anon, ok1 := pages["Anonymous pages"]
	wire, ok2 := pages["Pages wired down"]
	occupied, ok3 := pages["Pages occupied by compressor"]
	if page == 0 || !ok1 || !ok2 || !ok3 {
		return 0, 0, 0, false
	}
	return (anon - min(pages["Pages purgeable"], anon)) * page, wire * page, occupied * page, true
}

// smcNumber decodes an SMC value by its type (macOS): Apple silicon
// reports fans as little-endian floats, Intel Macs as big-endian fixed
// point.
func smcNumber(typ uint32, b []byte) (float64, error) {
	var name [4]byte
	binary.BigEndian.PutUint32(name[:], typ)
	switch t := string(name[:]); {
	case t == "flt " && len(b) >= 4:
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(b))), nil
	case t == "fpe2" && len(b) >= 2:
		return float64(binary.BigEndian.Uint16(b)) / 4, nil
	case t == "ui8 " && len(b) >= 1:
		return float64(b[0]), nil
	case t == "ui16" && len(b) >= 2:
		return float64(binary.BigEndian.Uint16(b)), nil
	case t == "ui32" && len(b) >= 4:
		return float64(binary.BigEndian.Uint32(b)), nil
	default:
		return 0, fmt.Errorf("smc type %q", t)
	}
}

// smiArgs asks nvidia-smi for every NVIDIA GPU's name, load, temperature
// and video memory (MiB).
var smiArgs = []string{"--query-gpu=name,utilization.gpu,temperature.gpu,memory.used,memory.total", "--format=csv,noheader,nounits"}

// nvidiaSMI reads the busiest GPU from nvidia-smi's output, one
// "NVIDIA GeForce RTX 3080, 37, 52, 6144, 10240" line per GPU ("[N/A]"
// where unknown).
func nvidiaSMI(out []byte) GPU {
	best := NoGPU
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 5 {
			continue
		}
		num := func(s string) (float64, bool) {
			v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			return v, err == nil && v >= 0
		}
		n := len(f)
		util, ok := num(f[n-4])
		if !ok || util <= best.Util {
			continue
		}
		g := NoGPU
		g.Name = strings.TrimSpace(strings.Join(f[:n-4], ","))
		g.Util = min(util, 100)
		g.Temp, _ = num(f[n-3])
		used, ok1 := num(f[n-2])
		total, ok2 := num(f[n-1])
		if ok1 && ok2 && total > 0 {
			g.Mem = Usage{Used: uint64(used) << 20, Total: uint64(total) << 20}
		}
		best = g
	}
	return best
}

// hwmonFans reads the fans under /sys/class/hwmon (Linux): fanN_input in
// rpm, and fanN_max where the driver knows it.
func hwmonFans(root string) []Fan {
	paths, _ := filepath.Glob(filepath.Join(root, "hwmon*", "fan*_input"))
	sort.Strings(paths)
	var fans []Fan
	for _, p := range paths {
		rpm, ok := readNumber(p)
		if !ok {
			continue
		}
		top, _ := readNumber(strings.TrimSuffix(p, "_input") + "_max")
		fans = append(fans, Fan{RPM: rpm, Max: top})
	}
	return fans
}

// amdGPU reads the busiest GPU under /sys/class/drm (Linux; AMD and some
// Intel drivers): gpu_busy_percent, and the video memory where told.
func amdGPU(root string) GPU {
	paths, _ := filepath.Glob(filepath.Join(root, "card*", "device", "gpu_busy_percent"))
	sort.Strings(paths)
	best := NoGPU
	for _, p := range paths {
		v, ok := readNumber(p)
		if !ok || v <= best.Util {
			continue
		}
		g := NoGPU
		g.Util = min(v, 100)
		dir := filepath.Dir(p)
		used, ok1 := readNumber(filepath.Join(dir, "mem_info_vram_used"))
		total, ok2 := readNumber(filepath.Join(dir, "mem_info_vram_total"))
		if ok1 && ok2 && total > 0 {
			g.Mem = Usage{Used: uint64(used), Total: uint64(total)}
		}
		best = g
	}
	return best
}

func readNumber(path string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	return v, err == nil && v >= 0
}
