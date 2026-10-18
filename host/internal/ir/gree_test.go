package ir

import (
	"slices"
	"testing"
)

// States from IRremoteESP8266's tests (test/ir_Gree_test.cpp).
var (
	// SetAndGetRaw: what a fresh remote sends.
	greeReset = Gree{0x00, 0x09, 0x20, 0x50, 0x00, 0x20, 0x00, 0x50}
	// NormalRealExample, a YAW1F remote: on, cool, 26°C, low fan, swing
	// position 2, display showing the outside temperature.
	greeYAW1F = Gree{0x19, 0x0A, 0x60, 0x50, 0x02, 0x23, 0x00, 0xF0}
	// Issue814Power, a YBOFB remote: on, cool, 23°C, low fan, auto swing.
	greeYBOFB = Gree{0x59, 0x07, 0x20, 0x50, 0x01, 0x20, 0x00, 0xC0}
)

func TestGreeFrame(t *testing.T) {
	if f := GreeDefault().Frame(GreeYAW1F); f != greeReset {
		t.Fatalf("default = %v, want %v", f, greeReset)
	}

	g := GreeDefault()
	g.SetPower(true)
	g.SetMode(ModeCool)
	g.SetTemp(26)
	g.SetFan(FanLow)
	if f := g.Frame(GreeYAW1F); f != (Gree{0x19, 0x0A, 0x60, 0x50, 0x00, 0x20, 0x00, 0xF0}) {
		t.Fatalf("cool 26 = %v", f)
	}
	g.SetTemp(23)
	if f := g.Frame(GreeYBOFB); f != (Gree{0x19, 0x07, 0x20, 0x50, 0x00, 0x20, 0x00, 0xC0}) {
		t.Fatalf("ybofb = %v", f) // no extra power bit
	}
	g.SetPower(false)
	if f := g.Frame(GreeYAW1F); f[2]&0x40 != 0 || f[0]&0x08 != 0 {
		t.Fatalf("off = %v", f)
	}
}

func TestGreeRules(t *testing.T) {
	g := GreeDefault()
	g.SetMode(ModeCool)
	g.SetTemp(40)
	if g.Temp() != GreeMaxTemp {
		t.Fatalf("temp %d", g.Temp())
	}
	g.SetTemp(3)
	if g.Temp() != GreeMinTemp {
		t.Fatalf("temp %d", g.Temp())
	}
	g.SetFan(FanHigh)
	g.SetMode(ModeAuto)
	if g.Temp() != GreeAutoTemp {
		t.Fatalf("auto temp %d", g.Temp())
	}
	g.SetTemp(18)
	if g.Temp() != GreeAutoTemp {
		t.Fatal("auto mode lets the temperature change")
	}
	g.SetMode(ModeDry)
	if g.Fan() != FanLow {
		t.Fatalf("dry fan %d", g.Fan())
	}
	g.SetFan(FanHigh)
	if g.Fan() != FanLow {
		t.Fatal("dry mode lets the fan change")
	}
	g.SetMode(Mode(7))
	if g.Mode() != ModeAuto {
		t.Fatalf("bad mode became %d", g.Mode())
	}
}

func TestGreeKeepsRemoteBits(t *testing.T) {
	g := greeYBOFB // auto swing, set with the remote
	g[1] |= 0x80   // and a timer
	g.SetTemp(25)
	f := g.Frame(GreeYBOFB)
	if f[0]&0x40 == 0 || f[4] != 0x01 {
		t.Fatalf("swing lost: %v", f)
	}
	if f[1]&0xF0 != 0 {
		t.Fatalf("timer kept: %v", f)
	}
}

func TestDecodeGree(t *testing.T) {
	for _, want := range []Gree{greeReset, greeYAW1F, greeYBOFB} {
		raw := GreeRaw(want)
		if len(raw) != GreeRawLen {
			t.Fatalf("%d timings", len(raw))
		}
		got, err := DecodeGree(raw)
		if err != nil || got != want {
			t.Fatalf("%v: got %v, %v", want, got, err)
		}
	}
	g, _ := DecodeGree(GreeRaw(greeYAW1F))
	if !g.Power() || g.Mode() != ModeCool || g.Temp() != 26 || g.Fan() != FanLow {
		t.Fatalf("yaw1f fields %v", g)
	}
	if m, ok := g.Model(); !ok || m != GreeYAW1F {
		t.Fatalf("model %q", m)
	}
	if m, _ := greeYBOFB.Model(); m != GreeYBOFB {
		t.Fatalf("model %q", m)
	}
	if _, ok := greeReset.Model(); ok {
		t.Fatal("model told from an off frame")
	}
}

func TestDecodeGreeTolerance(t *testing.T) {
	// A demodulating receiver stretches marks and shortens spaces.
	raw := GreeRaw(greeYAW1F)
	for i := range raw {
		if i%2 == 0 {
			raw[i] += 90
		} else {
			raw[i] -= 90
		}
	}
	raw = append(raw, 40000, 9000) // the start of a repeat
	if got, err := DecodeGree(raw); err != nil || got != greeYAW1F {
		t.Fatalf("jittered: %v, %v", got, err)
	}
}

func TestDecodeGreeRejects(t *testing.T) {
	good := GreeRaw(greeYAW1F)
	badSum := greeYAW1F
	badSum[7] ^= 0x10
	badFooter := slices.Clone(good)
	badFooter[2+64+1] = greeOneSpace // the footer's first bit, a space after the header and 32 bits, becomes 1
	for name, raw := range map[string][]int{
		"checksum":  GreeRaw(badSum),
		"footer":    badFooter,
		"truncated": good[:100],
		"other":     {4500, 4500, 560, 1690, 560, 560},
		"empty":     nil,
	} {
		if g, err := DecodeGree(raw); err == nil {
			t.Errorf("%s: accepted as %v", name, g)
		}
	}
}
