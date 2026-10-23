package perf

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
	"github.com/since2006/go-dock/host/internal/plugins"
	"github.com/since2006/go-dock/host/internal/sysmon"
)

const giB = 1 << 30

// mac is a reading on a Mac: everything known.
var mac = sysmon.Sample{
	CPU: 37.6, CPUUser: 25.2, CPUTemp: 51.4,
	GPU: sysmon.GPU{Name: "Apple M3 Max", Cores: 30, Util: 57, Renderer: 55, Tiler: 34},
	Mem: sysmon.Memory{Usage: sysmon.Usage{Used: 20 * giB, Total: 36 * giB}, App: 11 * giB, Wired: 4 * giB, Compressed: 5 * giB,
		Pressure: 1, Swap: sysmon.Usage{Used: 600 << 20, Total: 1 * giB}},
	Disk:    sysmon.Usage{Used: 900 * giB, Total: 1000 * giB},
	Fans:    []sysmon.Fan{{RPM: 0, Max: 6898}, {RPM: 0, Max: 7450}},
	Rx:      5.5 * 1024,
	Tx:      1.5 * 1024 * 1024,
	RxTotal: 12 * giB, TxTotal: 3 * giB,
	DiskRead: 2 << 20, DiskWrite: 512 << 10,
	Uptime: 3*24*time.Hour + 4*time.Hour + 5*time.Minute,
}

// saved records the orders a plugin saves.
type saved struct{ orders [][]string }

func (s *saved) save(order []string) error {
	s.orders = append(s.orders, order)
	return nil
}

func enabled() *Plugin {
	p := New(nil, (&saved{}).save, nil)
	p.SetOptions(Options{Enabled: true})
	return p
}

func item(t *testing.T, pg model.Page, id string) model.PageItem {
	t.Helper()
	for _, it := range pg.Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("no %s card in %+v", id, pg.Items)
	return model.PageItem{}
}

func ids(pg model.Page) []string {
	var out []string
	for _, it := range pg.Items {
		out = append(out, it.ID)
	}
	return out
}

func ptr(v int) *int { return &v }

func TestPage(t *testing.T) {
	p := enabled()
	pg, ok := p.Page()
	if !ok || pg.Title != "性能" || pg.Summary != "读取中…" || len(pg.Items) != 0 {
		t.Fatalf("page before a reading = %+v", pg)
	}
	p.add("Apple M3 Max", mac)
	pg, _ = p.Page()
	if pg.Summary != "已运行 3 天 4 小时" {
		t.Fatalf("summary = %q", pg.Summary)
	}
	if got, want := ids(pg), []string{"cpu", "gpu", "mem", "fan", "net", "disk"}; !reflect.DeepEqual(got, want) || !pg.Movable {
		t.Fatalf("cards %v, want %v, movable", got, want)
	}
	for _, it := range pg.Items {
		if it.Kind != model.KindMeter || it.Big != (it.ID == "cpu" || it.ID == "gpu") {
			t.Fatalf("%s: kind %s big %v", it.ID, it.Kind, it.Big)
		}
	}

	cpu := item(t, pg, "cpu")
	if cpu.Text != "38" || cpu.Unit != "%" || cpu.Note != "Apple M3 Max" || cpu.Sub != "51°C" || cpu.Level != "" {
		t.Fatalf("cpu = %+v", cpu)
	}
	wantRows := []model.Row{
		{Label: "用户", Text: "25%", Color: model.ColorBlue},
		{Label: "系统", Text: "12%", Color: model.ColorRed},
		{Label: "闲置", Text: "62%", Color: model.ColorGray},
	}
	if !reflect.DeepEqual(cpu.Rows, wantRows) {
		t.Fatalf("cpu rows = %+v", cpu.Rows)
	}
	gpu := item(t, pg, "gpu")
	if gpu.Text != "57" || gpu.Note != "Apple M3 Max" || gpu.Sub != "30 核" {
		t.Fatalf("gpu = %+v", gpu)
	}
	wantRows = []model.Row{
		{Label: "利用率", Text: "57%", Color: model.ColorBlue},
		{Label: "渲染", Text: "55%", Color: model.ColorRed},
		{Label: "Tiler", Text: "34%", Color: model.ColorTeal},
	}
	if !reflect.DeepEqual(gpu.Rows, wantRows) {
		t.Fatalf("gpu rows = %+v", gpu.Rows)
	}
	// One reading draws no chart.
	if cpu.Series != nil || gpu.Series != nil || item(t, pg, "net").Hist != nil {
		t.Fatal("a chart after one reading")
	}

	want := map[string]model.PageItem{
		"mem":  {Label: "内存", Icon: model.IconMemory, Text: "56", Unit: "%", Sub: "压缩 5.0G", Pct: ptr(56)},
		"fan":  {Label: "风扇", Icon: model.IconRotor, Text: "停转", Sub: "2 个", Pct: ptr(0)},
		"net":  {Label: "网络", Icon: model.IconNet, Text: "5.5", Unit: "KB/s", Sub: "↑ 1.5M"},
		"disk": {Label: "磁盘", Icon: model.IconDisk, Text: "90", Unit: "%", Sub: "剩 100G", Pct: ptr(90), Level: model.LevelWarn},
	}
	for id, w := range want {
		it := item(t, pg, id)
		it.ID, it.Kind = "", ""
		if !reflect.DeepEqual(it, w) {
			t.Errorf("%s = %+v, want %+v", id, it, w)
		}
	}
}

func TestCharts(t *testing.T) {
	p := enabled()
	for i := range History + 5 {
		s := mac
		s.CPU, s.CPUUser = float64(i*2), float64(i)
		s.GPU.Util, s.GPU.Renderer, s.GPU.Tiler = 60, 40, 20
		s.Rx = float64(i * 64 << 10)
		p.add("", s)
	}
	pg, _ := p.Page()
	cpu := item(t, pg, "cpu").Series
	if len(cpu) != 2 || cpu[0].Color != model.ColorBlue || cpu[1].Color != model.ColorRed {
		t.Fatalf("cpu series = %+v", cpu)
	}
	// User programs in front, user + system behind: the last History
	// readings.
	if user, busy := cpu[0].Hist, cpu[1].Hist; len(busy) != History || busy[0] != 10 || busy[History-1] != 68 || user[History-1] != 34 {
		t.Fatalf("cpu lines = %v / %v", user, busy)
	}
	gpu := item(t, pg, "gpu").Series
	if len(gpu) != 3 || gpu[0].Color != model.ColorTeal || gpu[0].Hist[0] != 20 || gpu[2].Color != model.ColorBlue || gpu[2].Hist[0] != 60 {
		t.Fatalf("gpu series = %+v", gpu)
	}
	// A busy network scales its line to its own top.
	net := item(t, pg, "net").Hist
	if net[History-1] != 100 || net[0] >= net[History-1] {
		t.Fatalf("net line = %v", net)
	}

	// An idle network stays flat near zero instead of filling the card.
	p = enabled()
	for range 3 {
		s := mac
		s.Rx = 2 << 10
		p.add("", s)
	}
	pg, _ = p.Page()
	if net := item(t, pg, "net").Hist; net[2] > 2 {
		t.Fatalf("idle network line = %v", net)
	}
}

// Other GPUs report their load and maybe video memory; a GPU with no
// load reading is left off.
func TestGPUs(t *testing.T) {
	p := enabled()
	s := mac
	s.GPU = sysmon.GPU{Name: "NVIDIA GeForce RTX 3080", Util: 37, Renderer: -1, Tiler: -1, Temp: 52,
		Mem: sysmon.Usage{Used: 6 * giB, Total: 10 * giB}}
	p.add("Intel Core i7-8700", s)
	p.add("Intel Core i7-8700", s)
	pg, _ := p.Page()
	gpu := item(t, pg, "gpu")
	if gpu.Note != "NVIDIA GeForce RTX 3080" || gpu.Sub != "52°C" {
		t.Fatalf("gpu = %+v", gpu)
	}
	if len(gpu.Series) != 1 || gpu.Series[0].Color != model.ColorBlue || len(gpu.Rows) != 2 || gpu.Rows[1].Text != "6.0/10G" {
		t.Fatalf("gpu chart %+v rows %+v", gpu.Series, gpu.Rows)
	}

	s.GPU = sysmon.NoGPU
	s.Fans, s.Rx, s.Tx, s.Disk = nil, -1, -1, sysmon.Usage{}
	p = enabled()
	p.add("Intel Core i7-8700", s)
	pg, _ = p.Page()
	if got, want := ids(pg), []string{"cpu", "mem", "net"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cards %v, want %v", got, want)
	}
	if net := item(t, pg, "net"); net.Text != "—" || net.Unit != "" || net.Sub != "" {
		t.Fatalf("net before a rate = %+v", net)
	}
}

// On a Mac the memory card's colour follows the memory pressure, and a
// Mac short of memory says so on top; elsewhere the percent decides.
func TestMemory(t *testing.T) {
	cases := []struct {
		name     string
		mem      sysmon.Memory
		level    string
		sub      string
		headline string
	}{
		{"mac, busy but fine", sysmon.Memory{Usage: sysmon.Usage{Used: 34 * giB, Total: 36 * giB}, Pressure: 1}, "", "34/36G", "已运行 3 天 4 小时"},
		{"mac, warn", sysmon.Memory{Usage: sysmon.Usage{Used: 30 * giB, Total: 36 * giB}, Compressed: 13 * giB, Pressure: 2,
			Swap: sysmon.Usage{Used: 600 << 20}}, model.LevelWarn, "压缩 13G", "内存紧张 · 交换 600M"},
		{"mac, critical", sysmon.Memory{Usage: sysmon.Usage{Used: 35 * giB, Total: 36 * giB}, Compressed: 13 * giB, Pressure: 4,
			Swap: sysmon.Usage{Used: 3 * giB / 2}}, model.LevelCrit, "压缩 13G", "内存严重不足 · 交换 1.5G"},
		{"linux", sysmon.Memory{Usage: sysmon.Usage{Used: 31 * giB, Total: 32 * giB}}, model.LevelCrit, "31/32G", "已运行 3 天 4 小时"},
	}
	for _, c := range cases {
		p := enabled()
		s := mac
		s.Mem = c.mem
		p.add("", s)
		pg, _ := p.Page()
		mem := item(t, pg, "mem")
		if mem.Level != c.level || mem.Sub != c.sub || pg.Summary != c.headline {
			t.Errorf("%s: level %q sub %q summary %q", c.name, mem.Level, mem.Sub, pg.Summary)
		}
	}
}

func TestLevels(t *testing.T) {
	p := enabled()
	s := mac
	s.CPU, s.GPU.Util = 95, 75
	p.add("", s)
	pg, _ := p.Page()
	for id, want := range map[string]string{"cpu": model.LevelCrit, "gpu": model.LevelWarn, "net": "", "fan": ""} {
		if got := item(t, pg, id).Level; got != want {
			t.Errorf("%s level = %q, want %q", id, got, want)
		}
	}
}

func TestDisabled(t *testing.T) {
	p := enabled()
	p.add("", mac)
	p.SetOptions(Options{})
	if _, ok := p.Page(); ok {
		t.Fatal("disabled page shown")
	}
	if err := p.Do(context.Background(), "cpu", model.ActToggle); !errors.Is(err, plugins.ErrNotFound) {
		t.Fatalf("Do on a disabled page = %v", err)
	}
	// Turned on again it starts over.
	p.SetOptions(Options{Enabled: true})
	if pg, _ := p.Page(); len(pg.Items) != 0 {
		t.Fatalf("old readings kept: %+v", pg.Items)
	}
	if err := p.Do(context.Background(), "cpu", model.ActToggle); !errors.Is(err, plugins.ErrBadAct) {
		t.Fatalf("a tap on a meter = %v", err)
	}
}

type fakeReader struct{ reads chan time.Time }

func (f *fakeReader) Read(context.Context) sysmon.Sample {
	f.reads <- time.Now()
	return mac
}

func (*fakeReader) Model() string { return "Apple M3 Max" }

// Run starts reading when the page is turned on; the first reading only
// starts the counts, and the next comes a whole Every later.
func TestRun(t *testing.T) {
	r := &fakeReader{reads: make(chan time.Time, 8)}
	changed := make(chan struct{}, 8)
	p := New(func() Reader { return r }, (&saved{}).save, func() { changed <- struct{}{} })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	select {
	case <-r.reads:
		t.Fatal("read while off")
	case <-time.After(50 * time.Millisecond):
	}
	p.SetOptions(Options{Enabled: true})
	var first time.Time
	select {
	case first = <-r.reads:
	case <-time.After(time.Second):
		t.Fatal("no reading after the page was turned on")
	}
	if pg, _ := p.Page(); len(pg.Items) != 0 {
		t.Fatal("the first reading was shown")
	}
	select {
	case second := <-r.reads:
		if gap := second.Sub(first); gap < Every*9/10 {
			t.Fatalf("second reading %v after the first", gap)
		}
	case <-time.After(Every + time.Second):
		t.Fatal("no second reading")
	}
	deadline := time.After(time.Second)
	for {
		if pg, _ := p.Page(); len(pg.Items) == 6 {
			break
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatal("second reading not shown")
		}
	}
}

func TestFormat(t *testing.T) {
	rates := map[float64][2]string{
		0:                {"0.0", "KB/s"},
		512:              {"0.5", "KB/s"},
		820 * 1024:       {"820", "KB/s"},
		999.6 * 1024:     {"1.0", "MB/s"},
		12.5 * (1 << 20): {"13", "MB/s"},
		3 * (1 << 30):    {"3.0", "GB/s"},
	}
	for b, want := range rates {
		if v, u := rate(b); v != want[0] || u != want[1] {
			t.Errorf("rate(%v) = %s %s, want %s %s", b, v, u, want[0], want[1])
		}
	}
	if s := size(97 * giB); s != "97G" {
		t.Errorf("size = %s", s)
	}
	if s := size(1200 * giB); s != "1.2T" {
		t.Errorf("size = %s", s)
	}
	if g := gib(5 * giB / 2); g != "2.5" {
		t.Errorf("gib = %s", g)
	}
	for d, want := range map[time.Duration]string{
		0:                "",
		42 * time.Minute: "已运行 42 分钟",
		5 * time.Hour:    "已运行 5 小时",
		50 * time.Hour:   "已运行 2 天 2 小时",
	} {
		if got := uptime(d); got != want {
			t.Errorf("uptime(%v) = %q, want %q", d, got, want)
		}
	}
}

// Every card can be big or small: dragged to the top row it shows a chart
// and a legend, below a reading, a note and a line or a bar.
func TestBigAndSmall(t *testing.T) {
	p := enabled()
	p.add("Apple M3 Max", mac)
	p.add("Apple M3 Max", mac)
	cases := []struct {
		id         string
		big, small model.PageItem // the fields that differ
	}{
		{"cpu", model.PageItem{Note: "Apple M3 Max", Sub: "51°C"}, model.PageItem{Sub: "51°C"}},
		{"gpu", model.PageItem{Note: "Apple M3 Max", Sub: "30 核"}, model.PageItem{Sub: "30 核"}},
		{"mem", model.PageItem{Note: "共 36G", Sub: "压力正常"}, model.PageItem{Sub: "压缩 5.0G"}},
		{"fan", model.PageItem{Note: "2 个风扇", Sub: "最高 7450"}, model.PageItem{Sub: "2 个"}},
		{"net", model.PageItem{Note: "累计 ↓12G ↑3.0G"}, model.PageItem{Sub: "↑ 1.5M"}},
		{"disk", model.PageItem{Note: "共 1.0T", Sub: "剩 100G"}, model.PageItem{Sub: "剩 100G"}},
	}
	rows := map[string][]string{
		"cpu":  {"用户 25%", "系统 12%", "闲置 62%"},
		"gpu":  {"利用率 57%", "渲染 55%", "Tiler 34%"},
		"mem":  {"App 11G", "联动 4.0G", "已压缩 5.0G", "交换 600M"},
		"fan":  {"风扇 1 停转", "风扇 2 停转"},
		"net":  {"下载 5.5 KB/s", "上传 1.5 MB/s"},
		"disk": {"读取 2.0 MB/s", "写入 512 KB/s"},
	}
	for _, c := range cases {
		big, ok := p.card(c.id, mac, true)
		small, _ := p.card(c.id, mac, false)
		if !ok || !big.Big || small.Big {
			t.Fatalf("%s: big %v small %v", c.id, big.Big, small.Big)
		}
		if big.Note != c.big.Note || big.Sub != c.big.Sub || small.Note != "" || small.Sub != c.small.Sub {
			t.Errorf("%s: big %q %q, small %q %q", c.id, big.Note, big.Sub, small.Note, small.Sub)
		}
		var got []string
		for _, r := range big.Rows {
			got = append(got, r.Label+" "+r.Text)
		}
		if !reflect.DeepEqual(got, rows[c.id]) || len(big.Series) == 0 {
			t.Errorf("%s: rows %v, %d series", c.id, got, len(big.Series))
		}
		if small.Rows != nil || small.Series != nil {
			t.Errorf("%s: small card with a legend or chart", c.id)
		}
	}
	// The memory chart stacks app memory, wired and compressed as shares
	// of the whole.
	mem, _ := p.card("mem", mac, true)
	if len(mem.Series) != 3 || mem.Series[0].Hist[0] != 31 || mem.Series[1].Hist[0] != 42 || mem.Series[2].Hist[0] != 56 {
		t.Fatalf("memory chart = %+v", mem.Series)
	}
	// Without the Mac's breakdown it charts what is used.
	m := mac.Mem
	m.App, m.Wired, m.Compressed, m.Pressure = 0, 0, 0, 0
	other, _ := p.card("mem", sysmon.Sample{Mem: m}, true)
	if other.Sub != "" || len(other.Rows) != 3 || other.Rows[0].Text != "20G" || other.Rows[1].Text != "16G" {
		t.Fatalf("memory elsewhere = %+v", other)
	}
}

// Dropping a card on another swaps them and saves the order; the first
// two cards the computer can fill are big.
func TestSwap(t *testing.T) {
	var s saved
	changed := 0
	p := New(nil, s.save, func() { changed++ })
	p.SetOptions(Options{Enabled: true})
	p.add("", mac)
	ctx := context.Background()

	if err := p.Swap(ctx, "mem", "gpu"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"cpu", "mem", "gpu", "fan", "net", "disk"}; len(s.orders) != 1 || !reflect.DeepEqual(s.orders[0], want) {
		t.Fatalf("saved %v, want %v", s.orders, want)
	}
	pg, _ := p.Page()
	if got := ids(pg); got[1] != "mem" || !item(t, pg, "mem").Big || item(t, pg, "gpu").Big {
		t.Fatalf("after the swap: %v, mem big %v", got, item(t, pg, "mem").Big)
	}
	if changed == 0 {
		t.Fatal("no change told")
	}

	// The config comes back with the order, as the app sends it.
	p.SetOptions(Options{Enabled: true, Order: s.orders[0]})
	if err := p.Swap(ctx, "disk", "cpu"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"disk", "mem", "gpu", "fan", "net", "cpu"}; !reflect.DeepEqual(s.orders[1], want) {
		t.Fatalf("saved %v, want %v", s.orders[1], want)
	}
	if err := p.Swap(ctx, "disk", "disk"); err != nil || len(s.orders) != 2 {
		t.Fatalf("a card dropped on itself: %v, %d saves", err, len(s.orders))
	}
	if err := p.Swap(ctx, "mem", "nope"); !errors.Is(err, plugins.ErrNotFound) {
		t.Fatalf("swap with an unknown card = %v", err)
	}

	// A card the computer cannot fill makes room: here no GPU, so the
	// next card is big.
	noGPU := mac
	noGPU.GPU = sysmon.NoGPU
	p.SetOptions(Options{Enabled: true, Order: []string{"gpu", "net"}})
	p.add("", noGPU)
	pg, _ = p.Page()
	if got, want := ids(pg), []string{"net", "cpu", "mem", "fan", "disk"}; !reflect.DeepEqual(got, want) || !pg.Items[1].Big || pg.Items[2].Big {
		t.Fatalf("cards %v, want %v with the first two big", got, want)
	}

	p.SetOptions(Options{})
	if err := p.Swap(ctx, "cpu", "mem"); !errors.Is(err, plugins.ErrNotFound) {
		t.Fatalf("swap on a disabled page = %v", err)
	}
}

func TestOrder(t *testing.T) {
	for _, c := range []struct{ in, want []string }{
		{nil, Cards},
		{[]string{"mem", "cpu"}, []string{"mem", "cpu", "gpu", "fan", "net", "disk"}},
		{[]string{"nope", "disk", "disk", "cpu"}, []string{"disk", "cpu", "gpu", "mem", "fan", "net"}},
	} {
		if got := Order(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Order(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// Changing only the order does not read out of turn.
func TestReorderKeepsPace(t *testing.T) {
	p := enabled()
	<-p.wake // turning the page on wakes it
	p.SetOptions(Options{Enabled: true, Order: []string{"mem"}})
	select {
	case <-p.wake:
		t.Fatal("a reorder woke the reader")
	default:
	}
}
