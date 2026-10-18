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
			{ID: "main", Kind: model.KindSwitch, Label: "办公室主灯", Text: "已开", On: ptr(true)},
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

	empty, _ := EncodePages(BuildPages(nil))
	if string(empty) != `{"t":"pages","pages":[]}`+"\n" {
		t.Fatalf("empty = %s", empty)
	}
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
	h, _ := DecodeDeviceLine([]byte(`{"t":"hello","proto":1,"pages":1}`))
	if h.Pages != 1 {
		t.Fatalf("%+v", h)
	}
}
