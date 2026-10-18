package device

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/since2006/go-dock/host/internal/model"
)

func TestEncodeIRSend(t *testing.T) {
	line, err := EncodeIRSend(38000, []int{9000, 4500, 620})
	if err != nil || string(line) != `{"t":"ir_send","hz":38000,"raw":[9000,4500,620]}`+"\n" {
		t.Fatalf("%s %v", line, err)
	}
	full := make([]int, maxIRRaw)
	for i := range full {
		full[i] = maxIRSpan
	}
	if line, err := EncodeIRSend(38000, full); err != nil || len(line) > MaxLine {
		t.Fatalf("longest frame: %d bytes, %v", len(line), err)
	}
	for name, c := range map[string]struct {
		hz  int
		raw []int
	}{
		"carrier":  {1000, []int{1}},
		"empty":    {38000, nil},
		"too long": {38000, make([]int, maxIRRaw+1)},
		"zero":     {38000, []int{620, 0}},
		"span":     {38000, []int{maxIRSpan + 1}},
	} {
		if _, err := EncodeIRSend(c.hz, c.raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestManagerIR(t *testing.T) {
	src := &fakeSource{snap: sampleSnapshot(), ch: make(chan struct{}, 1)}
	m, devices := newTestManager(t, src, io.Discard)
	heard := make(chan []int, 4)
	m.SetIRSink(func(raw []int) { heard <- raw })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	dev := <-devices
	dev.next(t, "hello")
	if err := m.SendIR(38000, []int{620}); err == nil || !strings.Contains(err.Error(), "未连接") {
		t.Fatalf("before hello: %v", err)
	}
	dev.send(t, `{"t":"ir_recv","raw":[9000,4500]}`) // before the handshake: ignored

	// Firmware without infrared.
	dev.send(t, `{"t":"hello","proto":1,"fw":"0.1.0","w":600,"h":450,"max_line":4096}`)
	waitFor(t, "ready", func() bool { return m.Status().State == StateReady })
	if err := m.SendIR(38000, []int{620}); err == nil || !strings.Contains(err.Error(), "不支持红外") {
		t.Fatalf("no ir: %v", err)
	}

	m.Reconnect()
	dev = <-devices
	dev.next(t, "hello")
	dev.send(t, `{"t":"hello","proto":1,"fw":"0.3.0","w":600,"h":450,"max_line":4096,"ir":1}`)
	waitFor(t, "ir status", func() bool { return m.Status().IR })
	if err := m.IRReady(); err != nil {
		t.Fatal(err)
	}
	if err := m.SendIR(38000, []int{9000, 4500, 620}); err != nil {
		t.Fatal(err)
	}
	msg := dev.next(t, "ir_send")
	if msg["hz"] != 38000.0 || len(msg["raw"].([]any)) != 3 {
		t.Fatalf("ir_send %v", msg)
	}
	if err := m.SendIR(38000, []int{0}); err == nil {
		t.Fatal("bad timing accepted")
	}

	dev.send(t, `{"t":"ir_recv","raw":[9010,4490,640]}`)
	select {
	case raw := <-heard:
		if !slices.Equal(raw, []int{9010, 4490, 640}) {
			t.Fatalf("heard %v", raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ir_recv not passed on")
	}
	select {
	case raw := <-heard:
		t.Fatalf("frame from before the handshake passed on: %v", raw)
	default:
	}
}

func TestBuildPagesIcons(t *testing.T) {
	f := BuildPages([]model.Page{{ID: "aircon", Title: "空调", Items: []model.PageItem{
		{ID: "temp", Kind: model.KindStepper, Label: "温度", Text: "30°C", AtMax: true},
		{ID: "mode", Kind: model.KindSelect, Label: "模式", Text: "制冷", Icon: model.IconCool},
		{ID: "fan", Kind: model.KindSelect, Label: "风速", Text: "自动", Icon: "Bad Icon"},
	}}})
	items := f.Pages[0].Items
	if !items[0].AtMax || items[0].AtMin || items[1].Icon != "cool" || items[2].Icon != "" {
		t.Fatalf("items %+v", items)
	}
	line, _ := EncodePages(f)
	if !strings.Contains(string(line), `"at_max":true`) || strings.Contains(string(line), `"at_min"`) {
		t.Fatalf("line %s", line)
	}
}
