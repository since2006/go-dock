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

func ptr(v int) *int { return &v }

// Readings are cut to fit the screen: texts shortened, pct held to 0–100,
// unknown levels and icons dropped, unusable ids and readings past
// maxReadings left out.
func TestBuildLine(t *testing.T) {
	in := []model.Reading{
		{ID: "cpu", Icon: model.IconCPU, Label: "CPU", Text: "28", Unit: "%", Pct: ptr(28), Level: model.LevelWarn},
		{ID: "mem", Icon: "Not An Icon", Label: "内存占用率", Text: "123456789", Unit: "MB/s!", Pct: ptr(130), Level: "loud"},
		{ID: "Bad ID", Label: "dropped"},
		{ID: "net", Icon: model.IconNet, Label: "网络", Text: "22", Unit: "KB/s", Sub: "↑ 1.5M 和更多"},
	}
	for i := range maxReadings {
		in = append(in, model.Reading{ID: "x" + string(rune('a'+i)), Label: "x", Text: "1"})
	}
	f := BuildLine(in)
	if f.T != "line" || len(f.Items) != maxReadings {
		t.Fatalf("line %+v", f)
	}
	cpu, mem, net := f.Items[0], f.Items[1], f.Items[2]
	if cpu.Icon != "cpu" || cpu.Level != "warn" || *cpu.Pct != 28 {
		t.Fatalf("cpu %+v", cpu)
	}
	if mem.Icon != "" || mem.Level != "" || *mem.Pct != 100 || mem.Label != "内存占…" || mem.Text != "1234567…" || mem.Unit != "MB/…" {
		t.Fatalf("mem %+v", mem)
	}
	if net.ID != "net" || net.Pct != nil || net.Sub != "↑ 1.5M …" {
		t.Fatalf("net %+v", net)
	}
	if empty := BuildLine(nil); empty.Items == nil {
		t.Fatal("no readings must still send an empty list")
	}
}

// maxReadings readings with every text at its longest fit one line.
func TestEncodeLineFitsLine(t *testing.T) {
	var in []model.Reading
	long := strings.Repeat("宽", 20)
	for i := range maxReadings {
		in = append(in, model.Reading{ID: strings.Repeat(string(rune('a'+i)), 16), Icon: strings.Repeat("i", 16),
			Label: long, Text: long, Unit: long, Sub: long, Pct: ptr(100), Level: model.LevelCrit})
	}
	line, err := EncodeLine(BuildLine(in))
	if err != nil || len(line) > MaxLine {
		t.Fatalf("%d bytes, %v", len(line), err)
	}
	var back Line
	if err := json.Unmarshal(line, &back); err != nil || len(back.Items) != maxReadings {
		t.Fatalf("%v %d", err, len(back.Items))
	}
}

type fakeLine struct {
	mu sync.Mutex
	rs []model.Reading
}

func (f *fakeLine) Line() []model.Reading {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rs
}

func (f *fakeLine) set(rs []model.Reading) {
	f.mu.Lock()
	f.rs = rs
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

func cpuLine(text string) []model.Reading {
	return []model.Reading{{ID: "cpu", Icon: model.IconCPU, Label: "CPU", Text: text, Unit: "%"}}
}

func TestManagerLine(t *testing.T) {
	src := &fakeSource{snap: sampleSnapshot(), ch: make(chan struct{}, 1)}
	line := &fakeLine{rs: cpuLine("28")}
	m, devices := newTestManager(t, src, io.Discard)
	m.SetLine(line)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	// Nothing before the handshake; then the line right after each
	// snapshot.
	dev := <-devices
	dev.next(t, "hello")
	dev.next(t, "snapshot")
	dev.none(t, "line", 100*time.Millisecond)
	dev.send(t, `{"t":"hello","proto":1,"fw":"0.2.0","w":600,"h":450}`)
	dev.next(t, "snapshot")
	l := dev.next(t, "line")
	if l["items"].([]any)[0].(map[string]any)["id"] != "cpu" {
		t.Fatalf("line %v", l)
	}

	// A change elsewhere does not resend an unchanged line ...
	src.set(sampleSnapshot())
	dev.next(t, "snapshot")
	dev.none(t, "line", 100*time.Millisecond)

	// ... a changed reading does, and so does the line going empty.
	line.set(cpuLine("35"))
	src.ch <- struct{}{}
	l = dev.next(t, "line")
	if item := l["items"].([]any)[0].(map[string]any); item["text"] != "35" {
		t.Fatalf("item %v", item)
	}
	line.set(nil)
	src.ch <- struct{}{}
	if l = dev.next(t, "line"); len(l["items"].([]any)) != 0 {
		t.Fatalf("emptied line %v", l)
	}
}

func TestManagerLineHeartbeat(t *testing.T) {
	src := &fakeSource{snap: sampleSnapshot(), ch: make(chan struct{}, 1)}
	m, devices := newTestManager(t, src, io.Discard)
	m.SetLine(&fakeLine{rs: cpuLine("28")})
	m.heartbeat = 30 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	dev := <-devices
	dev.send(t, `{"t":"hello","proto":1,"fw":"0.2.0","w":600,"h":450}`)
	dev.next(t, "line")
	dev.next(t, "line") // unchanged, from the heartbeat
}
