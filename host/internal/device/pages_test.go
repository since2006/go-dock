package device

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
)

func ptr(b bool) *bool { return &b }

func TestBuildPages(t *testing.T) {
	long := strings.Repeat("灯", 30)
	in := []model.Page{
		{ID: "lights", Title: "灯光控制面板很长了", Summary: long, Error: "MQTT 未连接", Items: []model.PageItem{
			{ID: "main", Kind: model.KindSwitch, Label: "办公室主灯", Text: "已开", On: ptr(true), Buttons: true},
			{ID: "desk", Kind: model.KindSwitch, Label: long, Text: "已关", On: ptr(false), Disabled: true},
			{ID: "x", Kind: model.KindSwitch, Label: "未知", Text: "未知"},
			{ID: "Bad ID", Kind: model.KindSwitch, Label: "dropped"},
			{ID: "kind", Kind: "", Label: "dropped"},
		}},
		{ID: "bad id", Title: "dropped"},
	}
	for i := range 6 {
		in = append(in, model.Page{ID: "p" + string(rune('a'+i)), Title: "x"})
	}
	f := BuildPages(in)
	if f.T != "pages" || len(f.Pages) != maxPages {
		t.Fatalf("pages %d: %+v", len(f.Pages), f)
	}
	p := f.Pages[0]
	if p.Title != "灯光控制面板很…" || p.Summary != strings.Repeat("灯", pageNoteLen-1)+"…" || p.Error != "MQTT 未连接" {
		t.Fatalf("page texts %+v", p)
	}
	if len(p.Items) != 3 || p.Items[1].Label != strings.Repeat("灯", itemLabelLen-1)+"…" {
		t.Fatalf("items %+v", p.Items)
	}
	if f.Pages[1].ID != "pa" {
		t.Fatalf("bad page id kept: %+v", f.Pages[1])
	}

	line, err := EncodePages(f)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Pages []struct {
			Items []map[string]any `json:"items"`
		} `json:"pages"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		t.Fatal(err)
	}
	items := raw.Pages[0].Items
	if items[0]["on"] != true || items[1]["on"] != false || items[1]["disabled"] != true {
		t.Fatalf("on/disabled %v", items)
	}
	if _, ok := items[2]["on"]; ok {
		t.Fatal("unknown state must omit on")
	}
	if _, ok := items[0]["busy"]; ok {
		t.Fatal("busy false must be omitted")
	}
	if items[0]["buttons"] != true {
		t.Fatalf("buttons %v", items[0])
	}
	if _, ok := items[1]["buttons"]; ok {
		t.Fatal("buttons false must be omitted")
	}

	empty, _ := EncodePages(BuildPages(nil))
	if string(empty) != `{"t":"pages","pages":[]}`+"\n" {
		t.Fatalf("empty = %s", empty)
	}
}

// A meter's reading: unit and sub cut to fit, pct and the line held to
// 0–100, the line to its last maxHist points, unknown levels dropped.
func TestBuildPagesMeter(t *testing.T) {
	pct := func(v int) *int { return &v }
	hist := make([]int, maxHist+5)
	for i := range hist {
		hist[i] = i * 4
	}
	in := []model.Page{{ID: "perf", Title: "性能", Items: []model.PageItem{
		{ID: "cpu", Kind: model.KindMeter, Icon: model.IconCPU, Label: "CPU", Text: "38", Unit: "%", Sub: "51°C",
			Pct: pct(38), Hist: []int{-5, 40, 120}, Level: model.LevelCrit},
		{ID: "net", Kind: model.KindMeter, Label: "网络", Text: "5.5", Unit: "KB/s/x", Sub: "↑ 1.5 MB/s up", Hist: hist, Level: "loud"},
		{ID: "disk", Kind: model.KindMeter, Label: "磁盘", Text: "90", Pct: pct(130)},
	}}}
	items := BuildPages(in).Pages[0].Items
	cpu, net, disk := items[0], items[1], items[2]
	if cpu.Unit != "%" || cpu.Sub != "51°C" || *cpu.Pct != 38 || cpu.Level != model.LevelCrit || cpu.Icon != "cpu" {
		t.Fatalf("cpu = %+v", cpu)
	}
	if len(cpu.Hist) != 3 || cpu.Hist[0] != 0 || cpu.Hist[1] != 40 || cpu.Hist[2] != 100 {
		t.Fatalf("cpu line = %v", cpu.Hist)
	}
	if net.Unit != "KB/…" || net.Sub != "↑ 1.5 M…" || net.Level != "" || net.Pct != nil {
		t.Fatalf("net = %+v", net)
	}
	if len(net.Hist) != maxHist || net.Hist[0] != 20 || net.Hist[maxHist-1] != 100 {
		t.Fatalf("net line = %v", net.Hist)
	}
	if *disk.Pct != 100 || disk.Hist != nil {
		t.Fatalf("disk = %+v", disk)
	}

	line, err := EncodePages(BuildPages(in))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(line); !strings.Contains(s, `"unit":"%","sub":"51°C","pct":38,"hist":[0,40,100],"level":"crit"`) || strings.Count(s, `"level"`) != 1 {
		t.Fatalf("line = %s", s)
	}

	// A big meter: its note, at most maxSeries series and maxRows rows,
	// colours the screen knows, a series without readings dropped.
	big := BuildPages([]model.Page{{ID: "perf", Title: "性能", Items: []model.PageItem{{
		ID: "cpu", Kind: model.KindMeter, Big: true, Label: "CPU", Text: "38", Note: "Apple M3 Max with a very long name",
		Series: []model.Series{{Color: model.ColorBlue, Hist: []int{1, 2}}, {Color: "pink", Hist: []int{3, 4}}, {Color: model.ColorRed},
			{Color: model.ColorTeal, Hist: []int{5, 6}}, {Color: model.ColorGray, Hist: []int{7, 8}}},
		Rows: []model.Row{{Label: "用户", Text: "28%", Color: model.ColorBlue}, {Label: "很长很长的名字啊", Text: "1234567890", Color: "pink"},
			{Label: "c"}, {Label: "d"}, {Label: "e"}},
	}}}}).Pages[0].Items[0]
	if p := BuildPages([]model.Page{{ID: "perf", Title: "性能", Movable: true}}).Pages[0]; !p.Movable {
		t.Fatalf("movable lost: %+v", p)
	}
	if !big.Big || big.Note != "Apple M3 Max wi…" {
		t.Fatalf("big = %+v", big)
	}
	if len(big.Series) != 2 || big.Series[0].C != "blue" || big.Series[1].C != "" || big.Series[1].Hist[0] != 3 {
		t.Fatalf("series = %+v", big.Series)
	}
	if len(big.Rows) != maxRows || big.Rows[0] != (RowFrame{Label: "用户", Text: "28%", C: "blue"}) || big.Rows[1].Label != "很长很长的…" || big.Rows[1].Text != "1234567…" || big.Rows[1].C != "" {
		t.Fatalf("rows = %+v", big.Rows)
	}
	// Other kinds carry none of it.
	if s, _ := EncodePages(BuildPages([]model.Page{{ID: "p", Title: "x", Items: []model.PageItem{{ID: "a", Kind: model.KindSwitch, Label: "灯"}}}})); strings.Contains(string(s), "hist") || strings.Contains(string(s), "pct") {
		t.Fatalf("switch = %s", s)
	}
}

// The 性能 dashboard with full charts fits one line next to the other
// pages at their usual sizes.
func TestPerfPageFitsLine(t *testing.T) {
	hist := func() []int {
		h := make([]int, maxHist)
		for i := range h {
			h[i] = 55 + i
		}
		return h
	}
	meter := func(id string, big bool, series int) model.PageItem {
		it := model.PageItem{ID: id, Kind: model.KindMeter, Big: big, Icon: model.IconCPU, Label: "CPU", Text: "100", Unit: "KB/s",
			Sub: "压缩 13G", Pct: func() *int { v := 100; return &v }(), Level: model.LevelWarn}
		if big {
			it.Note = "Apple M3 Max"
			for range series {
				it.Series = append(it.Series, model.Series{Color: model.ColorBlue, Hist: hist()})
			}
			it.Rows = []model.Row{{Label: "利用率", Text: "100%", Color: model.ColorBlue}, {Label: "渲染", Text: "100%", Color: model.ColorRed},
				{Label: "Tiler", Text: "100%", Color: model.ColorTeal}}
		} else {
			it.Hist = hist()
		}
		return it
	}
	lights := model.Page{ID: "lights", Title: "灯光", Summary: "3 / 6 盏亮着 · 1 盏未知"}
	for i := range 6 {
		lights.Items = append(lights.Items, model.PageItem{ID: "lamp" + string(rune('a'+i)), Kind: model.KindSwitch, Label: "办公室主灯", Text: "切换中…", On: ptr(true), Buttons: true, Busy: true})
	}
	aircon := model.Page{ID: "aircon", Title: "空调", Summary: "制冷 26°C · 自动风", Items: []model.PageItem{
		{ID: "power", Kind: model.KindSwitch, Label: "电源", Text: "已开", Icon: model.IconPower, On: ptr(true), Buttons: true},
		{ID: "temp", Kind: model.KindStepper, Label: "温度", Text: "26°C"},
		{ID: "mode", Kind: model.KindSelect, Label: "模式", Text: "制冷", Icon: model.IconCool},
		{ID: "fan", Kind: model.KindSelect, Label: "风速", Text: "自动", Icon: model.IconFan},
	}}
	screen := model.Page{ID: "screen", Title: "屏幕", Items: []model.PageItem{
		{ID: "bright", Kind: model.KindStepper, Label: "亮度", Text: "80%"},
		{ID: "dim", Kind: model.KindStepper, Label: "变暗后亮度", Text: "20%"},
		{ID: "dim_after", Kind: model.KindSelect, Label: "无任务后变暗", Text: "5 分钟", Icon: model.IconMoon},
		{ID: "beep", Kind: model.KindSwitch, Label: "提示音", Text: "已开", Icon: model.IconBell, On: ptr(true)},
	}}
	perf := model.Page{ID: "perf", Title: "性能", Summary: "内存严重不足 · 交换 1.5G", Items: []model.PageItem{
		meter("cpu", true, 2), meter("gpu", true, 3), meter("mem", false, 0), meter("fan", false, 0), meter("net", false, 0), meter("disk", false, 0),
	}}
	line, err := EncodePages(BuildPages([]model.Page{lights, aircon, screen, perf}))
	if err != nil {
		t.Fatal(err)
	}
	var got Pages
	_ = json.Unmarshal(line, &got)
	if len(got.Pages) != 4 || len(got.Pages[3].Items) != 6 {
		t.Fatalf("trimmed at %d bytes", len(line))
	}
	t.Logf("%d bytes", len(line))
}

func TestEncodePagesFitsLine(t *testing.T) {
	var ps []model.Page
	for i := range maxPages {
		p := model.Page{ID: "p" + string(rune('a'+i)), Title: strings.Repeat("页", 8), Summary: strings.Repeat("摘", 24), Error: strings.Repeat("错", 24)}
		for j := range maxItems {
			p.Items = append(p.Items, model.PageItem{ID: "item" + string(rune('a'+j)), Kind: "switch", Label: strings.Repeat("标", 12), Text: strings.Repeat("态", 8), On: ptr(true), Busy: true, Disabled: true})
		}
		ps = append(ps, p)
	}
	full, err := EncodePages(BuildPages(ps))
	if err != nil || len(full) > MaxLine {
		t.Fatalf("%d bytes, %v", len(full), err)
	}
	// Every text at its longest does not fit: items go from the tail.
	var got Pages
	_ = json.Unmarshal(full, &got)
	if len(got.Pages) != maxPages || len(got.Pages[0].Items) != maxItems || len(got.Pages[maxPages-1].Items) == maxItems {
		t.Fatalf("trimmed wrong: %d bytes, %d pages", len(full), len(got.Pages))
	}
}

type fakePages struct {
	mu    sync.Mutex
	pages []model.Page
	acts  chan model.Action
}

func (f *fakePages) Pages() []model.Page {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pages
}

func (f *fakePages) Do(ctx context.Context, a model.Action) error {
	f.acts <- a
	return nil
}

func (f *fakePages) set(ps []model.Page) {
	f.mu.Lock()
	f.pages = ps
	f.mu.Unlock()
}

// none fails if a message of type typ arrives within d.
func (d *fakeDevice) none(t *testing.T, typ string, dur time.Duration) {
	t.Helper()
	timeout := time.After(dur)
	for {
		select {
		case m, ok := <-d.lines:
			if !ok {
				return
			}
			if m["t"] == typ {
				t.Fatalf("unexpected %s: %v", typ, m)
			}
		case <-timeout:
			return
		}
	}
}

func lightsPage(text string) []model.Page {
	return []model.Page{{ID: "lights", Title: "灯光", Items: []model.PageItem{{ID: "main", Kind: "switch", Label: "主灯", Text: text}}}}
}

func TestManagerPages(t *testing.T) {
	src := &fakeSource{snap: sampleSnapshot(), ch: make(chan struct{}, 1)}
	pages := &fakePages{pages: lightsPage("已关"), acts: make(chan model.Action, 4)}
	m, devices := newTestManager(t, src, io.Discard)
	m.SetPages(pages)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	// Firmware without function pages gets none, and its taps are ignored.
	dev := <-devices
	dev.next(t, "hello")
	dev.send(t, `{"t":"action","page":"lights","item":"main","act":"toggle"}`) // before hello
	dev.send(t, `{"t":"hello","proto":1,"fw":"0.1.0","w":600,"h":450,"max_line":4096}`)
	dev.next(t, "snapshot")
	dev.none(t, "pages", 100*time.Millisecond)
	if m.Status().Pages {
		t.Fatal("status says pages")
	}
	select {
	case a := <-pages.acts:
		t.Fatalf("action before hello handled: %+v", a)
	default:
	}

	// Firmware with them gets pages right after each snapshot.
	m.Reconnect()
	dev = <-devices
	dev.next(t, "hello")
	dev.send(t, `{"t":"hello","proto":1,"fw":"0.2.0","w":600,"h":450,"max_line":4096,"ota":1,"pages":1}`)
	dev.next(t, "snapshot")
	p := dev.next(t, "pages")
	if p["pages"].([]any)[0].(map[string]any)["id"] != "lights" {
		t.Fatalf("pages %v", p)
	}
	waitFor(t, "pages status", func() bool { return m.Status().Pages })

	// A change elsewhere does not resend unchanged pages ...
	src.set(sampleSnapshot())
	dev.next(t, "snapshot")
	dev.none(t, "pages", 100*time.Millisecond)

	// ... a changed page does.
	pages.set(lightsPage("已开"))
	src.ch <- struct{}{}
	p = dev.next(t, "pages")
	item := p["pages"].([]any)[0].(map[string]any)["items"].([]any)[0].(map[string]any)
	if item["text"] != "已开" {
		t.Fatalf("item %v", item)
	}

	dev.send(t, `{"t":"action","page":"lights","item":"main","act":"toggle"}`)
	select {
	case a := <-pages.acts:
		if a != (model.Action{Page: "lights", Item: "main", Act: "toggle"}) {
			t.Fatalf("action %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("action not dispatched")
	}

	// A card dropped on another names it.
	dev.send(t, `{"t":"action","page":"perf","item":"mem","act":"swap","to":"gpu"}`)
	select {
	case a := <-pages.acts:
		if a != (model.Action{Page: "perf", Item: "mem", Act: "swap", To: "gpu"}) {
			t.Fatalf("swap %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("swap not dispatched")
	}
}

func TestManagerPagesHeartbeat(t *testing.T) {
	src := &fakeSource{snap: sampleSnapshot(), ch: make(chan struct{}, 1)}
	m, devices := newTestManager(t, src, io.Discard)
	m.SetPages(&fakePages{pages: lightsPage("已关")})
	m.heartbeat = 30 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	dev := <-devices
	dev.send(t, `{"t":"hello","proto":1,"fw":"0.2.0","w":600,"h":450,"max_line":4096,"pages":1}`)
	dev.next(t, "pages")
	dev.next(t, "pages") // unchanged, from the heartbeat
}

func TestDecodeAction(t *testing.T) {
	m, err := DecodeDeviceLine([]byte(`{"t":"action","page":"lights","item":"main","act":"on"}`))
	if err != nil || m.Page != "lights" || m.Item != "main" || m.Act != "on" {
		t.Fatalf("%+v %v", m, err)
	}
	s, err := DecodeDeviceLine([]byte(`{"t":"action","page":"perf","item":"mem","act":"swap","to":"gpu"}`))
	if err != nil || s.Act != "swap" || s.To != "gpu" {
		t.Fatalf("%+v %v", s, err)
	}
	h, _ := DecodeDeviceLine([]byte(`{"t":"hello","proto":1,"pages":1}`))
	if h.Pages != 1 {
		t.Fatalf("%+v", h)
	}
}
