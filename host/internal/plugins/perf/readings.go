package perf

import (
	"math"
	"slices"
	"strconv"

	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/sysmon"
)

// readings turns a sample into the line's items, in order, leaving out
// what the computer cannot report.
func readings(s sysmon.Sample) []model.Reading {
	out := []model.Reading{cpu(s)}
	if s.GPU >= 0 {
		out = append(out, gpu(s.GPU))
	}
	if s.Mem.Total > 0 {
		out = append(out, mem(s.Mem))
	}
	if len(s.Fans) > 0 {
		out = append(out, fan(s.Fans))
	}
	if s.Temp > 0 {
		out = append(out, temp(s.Temp))
	}
	if s.Disk.Total > 0 {
		out = append(out, disk(s.Disk))
	}
	return out
}

func cpu(s sysmon.Sample) model.Reading {
	return model.Reading{ID: "cpu", Icon: model.IconCPU, Label: "CPU", Text: round(s.CPU), Unit: "%", Pct: pct(s.CPU), Level: level(s.CPU, 70, 90)}
}

func gpu(v float64) model.Reading {
	return model.Reading{ID: "gpu", Icon: model.IconGPU, Label: "GPU", Text: round(v), Unit: "%", Pct: pct(v), Level: level(v, 70, 90)}
}

// mem is what programs hold. On a Mac its colour follows the system's
// memory pressure rather than the percent, which stays high on a Mac that
// is doing fine (it compresses memory before it swaps).
func mem(m sysmon.Memory) model.Reading {
	v := m.Pct()
	r := model.Reading{ID: "mem", Icon: model.IconMemory, Label: "内存", Text: round(v), Unit: "%", Pct: pct(v)}
	switch {
	case m.Pressure >= 4:
		r.Level = model.LevelCrit
	case m.Pressure >= 2:
		r.Level = model.LevelWarn
	case m.Pressure == 0:
		r.Level = level(v, 85, 95)
	}
	return r
}

// fan is the fastest fan's speed. It has no bar: four digits and rpm
// leave too little room for one.
func fan(fans []float64) model.Reading {
	r := model.Reading{ID: "fan", Icon: model.IconRotor, Label: "风扇", Text: "停转"}
	if rpm := slices.Max(fans); rpm >= 1 {
		r.Text, r.Unit = round(rpm), "rpm"
	}
	return r
}

// temp is the chip's temperature. Like the fan it has no bar: a percent
// of what would mean nothing.
func temp(c float64) model.Reading {
	return model.Reading{ID: "temp", Icon: model.IconTemp, Label: "温度", Text: round(c), Unit: "°C", Level: level(c, 85, 95)}
}

// disk is how full the system disk is.
func disk(d sysmon.Usage) model.Reading {
	v := d.Pct()
	return model.Reading{ID: "disk", Icon: model.IconDisk, Label: "磁盘", Text: round(v), Unit: "%", Pct: pct(v), Level: level(v, 90, 95)}
}

func round(v float64) string { return strconv.Itoa(int(math.Round(v))) }

func pct(v float64) *int {
	n := int(math.Round(min(max(v, 0), 100)))
	return &n
}

// level colours a reading from warn on amber, from crit on red.
func level(v, warn, crit float64) string {
	switch {
	case v >= crit:
		return model.LevelCrit
	case v >= warn:
		return model.LevelWarn
	}
	return ""
}
