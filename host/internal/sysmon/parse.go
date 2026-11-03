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

var ioregUtil = regexp.MustCompile(`"Device Utilization %"\s*=\s*(\d+)`)

// ioregGPU reads the busiest GPU's load from the output of `ioreg -r -d 1
// -c IOAccelerator` (macOS), one "+-o" entry per GPU; -1 when none says.
func ioregGPU(out []byte) float64 {
	best := -1.0
	for _, entry := range bytes.Split(out, []byte("+-o ")) {
		if m := ioregUtil.FindSubmatch(entry); m != nil {
			if v, err := strconv.ParseFloat(string(m[1]), 64); err == nil {
				best = max(best, min(v, 100))
			}
		}
	}
	return best
}

var (
	vmPageSize = regexp.MustCompile(`page size of (\d+) bytes`)
	vmLine     = regexp.MustCompile(`(?m)^"?([^":]+)"?:\s+(\d+)\.`)
)

// vmStat reads vm_stat's output (macOS) as Activity Monitor counts memory:
// app memory (anonymous pages that cannot be purged), wired and what the
// compressor occupies add up to 内存已使用.
func vmStat(out []byte) (used uint64, ok bool) {
	m := vmPageSize.FindSubmatch(out)
	if m == nil {
		return 0, false
	}
	page, _ := strconv.ParseUint(string(m[1]), 10, 64)
	pages := map[string]uint64{}
	for _, l := range vmLine.FindAllSubmatch(out, -1) {
		pages[string(l[1])], _ = strconv.ParseUint(string(l[2]), 10, 64)
	}
	anon, ok1 := pages["Anonymous pages"]
	wired, ok2 := pages["Pages wired down"]
	compressed, ok3 := pages["Pages occupied by compressor"]
	if page == 0 || !ok1 || !ok2 || !ok3 {
		return 0, false
	}
	return (anon - min(pages["Pages purgeable"], anon) + wired + compressed) * page, true
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

// smiArgs asks nvidia-smi for every NVIDIA GPU's load.
var smiArgs = []string{"--query-gpu=utilization.gpu", "--format=csv,noheader,nounits"}

// nvidiaSMI reads the busiest GPU's load from nvidia-smi's output, one
// number per GPU ("[N/A]" where unknown); -1 when none says.
func nvidiaSMI(out []byte) float64 {
	best := -1.0
	for _, line := range strings.Split(string(out), "\n") {
		if v, err := strconv.ParseFloat(strings.TrimSpace(line), 64); err == nil && v >= 0 {
			best = max(best, min(v, 100))
		}
	}
	return best
}

// hwmonFans reads the fans' speeds under /sys/class/hwmon (Linux):
// fanN_input in rpm.
func hwmonFans(root string) []float64 {
	paths, _ := filepath.Glob(filepath.Join(root, "hwmon*", "fan*_input"))
	sort.Strings(paths)
	var fans []float64
	for _, p := range paths {
		if rpm, ok := readNumber(p); ok {
			fans = append(fans, rpm)
		}
	}
	return fans
}

// amdGPU reads the busiest GPU's load under /sys/class/drm (Linux; AMD and
// some Intel drivers); -1 when none says.
func amdGPU(root string) float64 {
	paths, _ := filepath.Glob(filepath.Join(root, "card*", "device", "gpu_busy_percent"))
	best := -1.0
	for _, p := range paths {
		if v, ok := readNumber(p); ok {
			best = max(best, min(v, 100))
		}
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
