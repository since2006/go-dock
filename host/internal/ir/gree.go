package ir

import (
	"errors"
	"fmt"
)

// Gree air conditioners, after IRremoteESP8266 (ir_Gree.cpp): an 8-byte
// state sent as two blocks of four bytes, each byte least significant bit
// first, with a 3-bit footer and a long gap between the blocks.
const (
	greeHdrMark   = 9000
	greeHdrSpace  = 4500
	greeBitMark   = 620
	greeOneSpace  = 1600
	greeZeroSpace = 540
	greeMsgSpace  = 19980
	greeFooter    = 0b010

	// GreeRawLen is the number of timings in one frame.
	GreeRawLen = 2 + 32*2 + 3*2 + 2 + 32*2 + 1

	GreeMinTemp = 16
	GreeMaxTemp = 30
	// GreeAutoTemp is the temperature auto mode is locked to.
	GreeAutoTemp = 25
)

// Mode is an operating mode.
type Mode uint8

const (
	ModeAuto Mode = 0
	ModeCool Mode = 1
	ModeDry  Mode = 2
	ModeFan  Mode = 3
	ModeHeat Mode = 4
)

// Fan is a fan speed.
type Fan uint8

const (
	FanAuto Fan = 0
	FanLow  Fan = 1
	FanMed  Fan = 2
	FanHigh Fan = 3
)

// GreeModel is the remote a frame imitates; they differ in how they mark
// power on.
type GreeModel string

const (
	GreeYAW1F GreeModel = "yaw1f" // most remotes: an extra bit is set while on
	GreeYBOFB GreeModel = "ybofb"
)

var (
	errNotGree   = errors.New("不是格力空调的红外信号")
	errGreeCheck = errors.New("格力红外信号的校验和不对")
)

// Gree is a Gree remote's state. Bits go-dock does not set (swing, light,
// sleep, ...) keep what the last frame said, so a tap on the screen does
// not undo what was set with the remote.
type Gree [8]byte

// GreeDefault is the state of a fresh remote: off, auto, 25°C, auto fan,
// display light on.
func GreeDefault() Gree {
	return Gree{0x00, 0x09, 0x20, 0x50, 0x00, 0x20, 0x00, 0x00}
}

func (g Gree) Power() bool { return g[0]&0x08 != 0 }
func (g Gree) Mode() Mode  { return Mode(g[0] & 0x07) }
func (g Gree) Fan() Fan    { return Fan(g[0] >> 4 & 0x03) }
func (g Gree) Temp() int   { return int(g[1]&0x0F) + GreeMinTemp }

func (g *Gree) SetPower(on bool) {
	g[0] &^= 0x08
	if on {
		g[0] |= 0x08
	}
}

// SetMode sets the mode; auto locks the temperature, dry the fan.
func (g *Gree) SetMode(m Mode) {
	if m > ModeHeat {
		m = ModeAuto
	}
	g[0] = g[0]&^0x07 | byte(m)
	switch m {
	case ModeAuto:
		g.setTemp(GreeAutoTemp)
	case ModeDry:
		g.setFan(FanLow)
	}
}

// SetTemp sets the temperature in °C, kept to 16–30 and to 25 in auto.
func (g *Gree) SetTemp(c int) {
	if g.Mode() == ModeAuto {
		c = GreeAutoTemp
	}
	g.setTemp(c)
}

func (g *Gree) setTemp(c int) {
	c = max(GreeMinTemp, min(GreeMaxTemp, c))
	g[1] = g[1]&^0x0F | byte(c-GreeMinTemp)
	g[3] &^= 0x0C // whole degrees Celsius
}

// SetFan sets the fan speed; dry mode keeps it low.
func (g *Gree) SetFan(f Fan) {
	if g.Mode() == ModeDry {
		f = FanLow
	}
	g.setFan(f)
}

func (g *Gree) setFan(f Fan) {
	g[0] = g[0]&^0x30 | byte(min(f, FanHigh))<<4
}

// Model tells the remote model from a frame sent while on.
func (g Gree) Model() (GreeModel, bool) {
	if !g.Power() {
		return "", false
	}
	if g[2]&0x40 != 0 {
		return GreeYAW1F, true
	}
	return GreeYBOFB, true
}

// Frame is the state as sent: the power bits the model uses, no timer (a
// remote's timer is not go-dock's to repeat) and the checksum.
func (g Gree) Frame(model GreeModel) Gree {
	f := g
	f[2] &^= 0x40
	if f.Power() && model != GreeYBOFB {
		f[2] |= 0x40
	}
	f[1] &^= 0xF0 // timer: half hour, tens, enabled
	f[2] &^= 0x0F // timer: hours
	f[7] = f[7]&0x0F | greeChecksum(f)<<4
	return f
}

func (g Gree) String() string { return fmt.Sprintf("% X", g[:]) }

func greeChecksum(g Gree) byte {
	sum := 10
	for _, b := range g[:4] {
		sum += int(b & 0x0F)
	}
	for _, b := range g[4:7] {
		sum += int(b >> 4)
	}
	return byte(sum & 0x0F)
}

// GreeRaw is the frame as timings to send at Carrier.
func GreeRaw(f Gree) []int {
	raw := make([]int, 0, GreeRawLen)
	bits := func(v byte, n int) {
		for i := range n {
			space := greeZeroSpace
			if v>>i&1 == 1 {
				space = greeOneSpace
			}
			raw = append(raw, greeBitMark, space)
		}
	}
	raw = append(raw, greeHdrMark, greeHdrSpace)
	for _, b := range f[:4] {
		bits(b, 8)
	}
	bits(greeFooter, 3)
	raw = append(raw, greeBitMark, greeMsgSpace)
	for _, b := range f[4:] {
		bits(b, 8)
	}
	return append(raw, greeBitMark)
}

// DecodeGree reads a Gree frame from received timings; anything after the
// frame (a repeat, noise) is ignored.
func DecodeGree(raw []int) (Gree, error) {
	var g Gree
	d := decoder{raw: raw}
	if !d.pair(greeHdrMark, greeHdrSpace) {
		return g, errNotGree
	}
	for i := range 4 {
		v, ok := d.bits(8, greeBitMark, greeOneSpace, greeZeroSpace)
		if !ok {
			return g, errNotGree
		}
		g[i] = byte(v)
	}
	if v, ok := d.bits(3, greeBitMark, greeOneSpace, greeZeroSpace); !ok || v != greeFooter {
		return g, errNotGree
	}
	if !d.pair(greeBitMark, greeMsgSpace) {
		return g, errNotGree
	}
	for i := 4; i < 8; i++ {
		v, ok := d.bits(8, greeBitMark, greeOneSpace, greeZeroSpace)
		if !ok {
			return g, errNotGree
		}
		g[i] = byte(v)
	}
	if !d.mark(greeBitMark) {
		return g, errNotGree
	}
	if g[7]>>4 != greeChecksum(g) {
		return g, errGreeCheck
	}
	return g, nil
}
