package perf

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/sysmon"
)

// The smallest tops of the network and disk charts, so an idle network or
// disk draws a flat line instead of magnifying a trickle.
const (
	netFloor  = 128 << 10 // bytes per second
	diskFloor = 1 << 20
)

// card draws one card big or small; false when the computer cannot fill
// it. A big card has a note line, a chart of stacked series and a legend;
// a small one its reading, a note, and a line or a bar.
func (p *Plugin) card(id string, s sysmon.Sample, big bool) (model.PageItem, bool) {
	switch id {
	case "cpu":
		return p.cpuCard(s, big), true
	case "gpu":
		return p.gpuCard(s.GPU, big), s.GPU.Util >= 0
	case "mem":
		return p.memCard(s.Mem, big), s.Mem.Total > 0
	case "fan":
		return p.fanCard(s.Fans, big), len(s.Fans) > 0
	case "net":
		return p.netCard(s, big), true
	case "disk":
		return p.diskCard(s, big), s.Disk.Total > 0
	}
	return model.PageItem{}, false
}

func meter(id, icon, label string, big bool) model.PageItem {
	return model.PageItem{ID: id, Kind: model.KindMeter, Icon: icon, Label: label, Big: big}
}

// cpuCard charts the user programs' share stacked under the system's.
func (p *Plugin) cpuCard(s sysmon.Sample, big bool) model.PageItem {
	it := meter("cpu", model.IconCPU, "CPU", big)
	it.Text, it.Unit, it.Sub, it.Level = round(s.CPU), "%", temp(s.CPUTemp), level(s.CPU, 70, 90)
	if !big {
		it.Hist = p.line("cpu", 100)
		return it
	}
	it.Note = p.model
	it.Series = p.chart(100, model.ColorBlue, "cpu_user", model.ColorRed, "cpu")
	it.Rows = []model.Row{
		{Label: "用户", Text: round(s.CPUUser) + "%", Color: model.ColorBlue},
		{Label: "系统", Text: round(s.CPU-s.CPUUser) + "%", Color: model.ColorRed},
		{Label: "闲置", Text: round(100-s.CPU) + "%", Color: model.ColorGray},
	}
	return it
}

// gpuCard charts an Apple GPU's renderer and tiler inside its load; other
// GPUs have only their load, and maybe their video memory.
func (p *Plugin) gpuCard(g sysmon.GPU, big bool) model.PageItem {
	it := meter("gpu", model.IconGPU, "GPU", big)
	it.Text, it.Unit, it.Sub, it.Level = round(g.Util), "%", temp(g.Temp), level(g.Util, 70, 90)
	if g.Cores > 0 {
		it.Sub = fmt.Sprintf("%d 核", g.Cores)
	}
	if !big {
		it.Hist = p.line("gpu", 100)
		return it
	}
	it.Note = g.Name
	if it.Note == "" {
		it.Note = p.model
	}
	it.Rows = []model.Row{{Label: "利用率", Text: round(g.Util) + "%", Color: model.ColorBlue}}
	if g.Renderer >= 0 && g.Tiler >= 0 {
		it.Series = p.chart(100, model.ColorTeal, "gpu_tiler", model.ColorRed, "gpu_renderer", model.ColorBlue, "gpu")
		it.Rows = append(it.Rows,
			model.Row{Label: "渲染", Text: round(g.Renderer) + "%", Color: model.ColorRed},
			model.Row{Label: "Tiler", Text: round(g.Tiler) + "%", Color: model.ColorTeal})
		return it
	}
	it.Series = p.chart(100, model.ColorBlue, "gpu")
	if g.Mem.Total > 0 {
		it.Rows = append(it.Rows, model.Row{Label: "显存", Text: gib(g.Mem.Used) + "/" + gib(g.Mem.Total) + "G", Color: model.ColorPurple})
	}
	return it
}

// memCard shows what programs hold. A Mac short of memory compresses it
// before it swaps, so there the small card's note is how much is
// compressed, the big one charts app memory, wired and compressed, and
// the colour follows the system's memory pressure rather than the
// percent, which stays high on a Mac that is doing fine.
func (p *Plugin) memCard(m sysmon.Memory, big bool) model.PageItem {
	v := m.Pct()
	it := meter("mem", model.IconMemory, "内存", big)
	it.Text, it.Unit = round(v), "%"
	switch {
	case m.Pressure >= 4:
		it.Level = model.LevelCrit
	case m.Pressure >= 2:
		it.Level = model.LevelWarn
	case m.Pressure == 0:
		it.Level = level(v, 85, 95)
	}
	if !big {
		it.Sub, it.Pct = gib(m.Used)+"/"+gib(m.Total)+"G", pct(v)
		if m.Compressed >= 1<<30 {
			it.Sub = "压缩 " + size(m.Compressed)
		}
		return it
	}
	it.Note = "共 " + size(m.Total)
	it.Sub = map[int]string{1: "压力正常", 2: "压力偏高", 4: "压力严重"}[m.Pressure]
	if m.App+m.Wired+m.Compressed > 0 {
		it.Series = p.chart(100, model.ColorBlue, "mem_app", model.ColorOrange, "mem_app_wired", model.ColorPurple, "mem")
		it.Rows = []model.Row{
			{Label: "App", Text: size(m.App), Color: model.ColorBlue},
			{Label: "联动", Text: size(m.Wired), Color: model.ColorOrange},
			{Label: "已压缩", Text: size(m.Compressed), Color: model.ColorPurple},
			{Label: "交换", Text: bytesText(m.Swap.Used), Color: model.ColorGray},
		}
		return it
	}
	it.Series = p.chart(100, model.ColorBlue, "mem")
	it.Rows = []model.Row{
		{Label: "已用", Text: size(m.Used), Color: model.ColorBlue},
		{Label: "可用", Text: size(m.Total - m.Used), Color: model.ColorGray},
	}
	if m.Swap.Total > 0 {
		it.Rows = append(it.Rows, model.Row{Label: "交换", Text: bytesText(m.Swap.Used), Color: model.ColorPurple})
	}
	return it
}

// fanCard shows the fastest fan; the big one lists every fan.
func (p *Plugin) fanCard(fans []sysmon.Fan, big bool) model.PageItem {
	f := fastest(fans)
	it := meter("fan", model.IconRotor, "风扇", big)
	it.Text = "停转"
	if f.RPM >= 1 {
		it.Text, it.Unit = round(f.RPM), "rpm"
	}
	if !big {
		if len(fans) > 1 {
			it.Sub = fmt.Sprintf("%d 个", len(fans))
		}
		if f.Max > 0 {
			it.Pct = pct(f.RPM / f.Max * 100)
		}
		return it
	}
	it.Note = fmt.Sprintf("%d 个风扇", len(fans))
	if f.Max > 0 {
		it.Sub = "最高 " + round(f.Max)
	}
	it.Series = p.chart(f.Max, model.ColorTeal, "fan")
	colors := []string{model.ColorTeal, model.ColorBlue, model.ColorPurple, model.ColorOrange}
	for i, x := range fans[:min(len(fans), len(colors))] {
		it.Rows = append(it.Rows, model.Row{Label: fmt.Sprintf("风扇 %d", i+1), Text: rpm(x.RPM), Color: colors[i]})
	}
	return it
}

// netCard shows the download speed; the big one stacks the upload on it
// and tells what the computer moved since it started.
func (p *Plugin) netCard(s sysmon.Sample, big bool) model.PageItem {
	it := meter("net", model.IconNet, "网络", big)
	it.Text = "—"
	if s.Rx >= 0 {
		it.Text, it.Unit = rate(s.Rx)
	}
	if !big {
		if s.Rx >= 0 {
			v, u := rate(s.Tx)
			it.Sub = "↑ " + v + u[:1]
			it.Hist = p.line("net", netFloor)
		}
		return it
	}
	if s.RxTotal > 0 || s.TxTotal > 0 {
		it.Note = "累计 ↓" + bytesText(s.RxTotal) + " ↑" + bytesText(s.TxTotal)
	}
	it.Series = p.chart(max(netFloor, p.peak("net_sum")), model.ColorBlue, "net", model.ColorOrange, "net_sum")
	it.Rows = []model.Row{
		{Label: "下载", Text: rateText(s.Rx), Color: model.ColorBlue},
		{Label: "上传", Text: rateText(s.Tx), Color: model.ColorOrange},
	}
	return it
}

// diskCard shows how full the system disk is; the big one also charts
// reading stacked under writing.
func (p *Plugin) diskCard(s sysmon.Sample, big bool) model.PageItem {
	d := s.Disk.Pct()
	it := meter("disk", model.IconDisk, "磁盘", big)
	it.Text, it.Unit, it.Level = round(d), "%", level(d, 90, 95)
	free := "剩 " + size(s.Disk.Total-s.Disk.Used)
	if !big {
		it.Sub, it.Pct = free, pct(d)
		return it
	}
	it.Note, it.Sub = "共 "+size(s.Disk.Total), free
	it.Series = p.chart(max(diskFloor, p.peak("disk_sum")), model.ColorBlue, "disk_read", model.ColorOrange, "disk_sum")
	it.Rows = []model.Row{
		{Label: "读取", Text: rateText(s.DiskRead), Color: model.ColorBlue},
		{Label: "写入", Text: rateText(s.DiskWrite), Color: model.ColorOrange},
	}
	return it
}

// summary is how long the computer has been on, unless it is short of
// memory: then that, and how much it has swapped out.
func summary(s sysmon.Sample) string {
	short := ""
	switch {
	case s.Mem.Pressure >= 4:
		short = "内存严重不足"
	case s.Mem.Pressure >= 2:
		short = "内存紧张"
	default:
		return uptime(s.Uptime)
	}
	if s.Mem.Swap.Used >= 1<<20 {
		short += " · 交换 " + bytesText(s.Mem.Swap.Used)
	}
	return short
}

// uptime says how long the computer has been on: 已运行 3 天 4 小时.
func uptime(d time.Duration) string {
	switch {
	case d <= 0:
		return ""
	case d < time.Hour:
		return fmt.Sprintf("已运行 %d 分钟", d/time.Minute)
	case d < 24*time.Hour:
		return fmt.Sprintf("已运行 %d 小时", d/time.Hour)
	default:
		return fmt.Sprintf("已运行 %d 天 %d 小时", d/(24*time.Hour), d%(24*time.Hour)/time.Hour)
	}
}

// chart makes a big card's series from colour, history key pairs scaled
// against top, the first in front; none before two readings.
func (p *Plugin) chart(top float64, pairs ...string) []model.Series {
	var out []model.Series
	for i := 0; i+1 < len(pairs); i += 2 {
		if h := p.line(pairs[i+1], top); h != nil {
			out = append(out, model.Series{Color: pairs[i], Hist: h})
		}
	}
	return out
}

// line scales a chart's readings to 0–100 against top, or against the
// highest of them when that is higher (a busy network) or top is unknown.
func (p *Plugin) line(k string, top float64) []int {
	h := p.hist[k]
	if len(h) < 2 {
		return nil // one point makes no line
	}
	top = max(top, p.peak(k))
	out := make([]int, len(h))
	if top <= 0 {
		return out
	}
	for i, v := range h {
		out[i] = int(math.Round(min(max(v/top*100, 0), 100)))
	}
	return out
}

// peak is the highest reading a chart holds.
func (p *Plugin) peak(k string) float64 {
	top := 0.0
	for _, v := range p.hist[k] {
		top = max(top, v)
	}
	return top
}

func fastest(fans []sysmon.Fan) sysmon.Fan {
	var f sysmon.Fan
	for _, x := range fans {
		f.RPM, f.Max = max(f.RPM, x.RPM), max(f.Max, x.Max)
	}
	return f
}

func round(v float64) string { return strconv.Itoa(int(math.Round(v))) }

func pct(v float64) *int {
	n := int(math.Round(min(max(v, 0), 100)))
	return &n
}

func temp(c float64) string {
	if c <= 0 {
		return ""
	}
	return round(c) + "°C"
}

// rpm prints a fan's speed: 2150 rpm, or 停转.
func rpm(v float64) string {
	if v < 1 {
		return "停转"
	}
	return round(v) + " rpm"
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

// short prints at most three digits: 0.4, 9.5, 12, 820.
func short(v float64) string {
	if v < 9.95 {
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return round(v)
}

// rate splits a speed in bytes per second into a number and its unit.
func rate(b float64) (string, string) {
	units := []string{"KB/s", "MB/s", "GB/s"}
	v, i := b/1024, 0
	for v >= 999.5 && i < len(units)-1 {
		v, i = v/1024, i+1
	}
	return short(v), units[i]
}

// rateText prints a speed whole: 22 KB/s, or — while unknown.
func rateText(b float64) string {
	if b < 0 {
		return "—"
	}
	v, u := rate(b)
	return v + " " + u
}

// gib prints bytes in GiB without the unit: 5.2, 36.
func gib(b uint64) string { return short(float64(b) / (1 << 30)) }

// bytesText prints bytes in MiB below 1 GiB: 594M, 2.1G.
func bytesText(b uint64) string {
	if b < 1<<30 {
		return short(float64(b)/(1<<20)) + "M"
	}
	return size(b)
}

// size prints bytes in GiB, or TiB from 1000 GiB: 97G, 1.2T.
func size(b uint64) string {
	if g := float64(b) / (1 << 30); g < 999.5 {
		return short(g) + "G"
	}
	return short(float64(b)/(1<<40)) + "T"
}
