package device

import (
	"errors"
	"fmt"
	"time"
)

// Infrared through the screen (docs/protocol-v1.md §3.7, §4.6): go-dock
// encodes frames as raw timings, the screen only sends and receives them.

// IRSend makes the device send one infrared frame.
type IRSend struct {
	T   string `json:"t"`
	Hz  int    `json:"hz"`
	Raw []int  `json:"raw"` // µs, mark and space in turn, from a mark
}

// irLine is a frame waiting for the serial loop; one left over from an
// earlier connection is dropped rather than sent late.
type irLine struct {
	line []byte
	at   time.Time
}

const irStale = 2 * time.Second

const (
	maxIRRaw  = 512   // timings in one frame, either way
	maxIRSpan = 32767 // µs, the longest timing the device's RMT holds
	minIRHz   = 30000
	maxIRHz   = 60000
)

// EncodeIRSend encodes the ir_send message. Timings the device cannot
// send are an error, not a frame it would mangle.
func EncodeIRSend(hz int, raw []int) ([]byte, error) {
	if hz < minIRHz || hz > maxIRHz {
		return nil, fmt.Errorf("红外载波 %d Hz 超出范围", hz)
	}
	if len(raw) == 0 || len(raw) > maxIRRaw {
		return nil, fmt.Errorf("红外信号有 %d 段，超出范围", len(raw))
	}
	for _, d := range raw {
		if d < 1 || d > maxIRSpan {
			return nil, fmt.Errorf("红外信号里有 %d µs 的一段，超出范围", d)
		}
	}
	return encodeLine(IRSend{T: "ir_send", Hz: hz, Raw: raw})
}

// IRReady says why the screen cannot send infrared now, or nil.
func (m *Manager) IRReady() error {
	switch st := m.Status(); {
	case st.Flash.Busy():
		return errors.New("屏幕正在升级固件")
	case st.State != StateReady:
		return errors.New("屏幕未连接")
	case !st.IR:
		return errors.New("屏幕固件不支持红外，请先升级固件")
	}
	return nil
}

// SendIR sends one frame through the screen. It fails at once when the
// screen cannot send now; a frame never goes out late.
func (m *Manager) SendIR(hz int, raw []int) error {
	if err := m.IRReady(); err != nil {
		return err
	}
	line, err := EncodeIRSend(hz, raw)
	if err != nil {
		return err
	}
	select {
	case m.irReq <- irLine{line, time.Now()}:
		return nil
	default:
		return errors.New("红外发送太频繁，请稍后再试")
	}
}

// SetIRSink sets who hears the frames the screen's receiver picks up;
// call it before Run. It runs on the serial loop and must not block.
func (m *Manager) SetIRSink(f func(raw []int)) {
	m.mu.Lock()
	m.irSink = f
	m.mu.Unlock()
}

func (m *Manager) irReceived(raw []int) {
	m.mu.Lock()
	f := m.irSink
	m.mu.Unlock()
	if f == nil || len(raw) == 0 || len(raw) > maxIRRaw {
		return
	}
	m.log.Debug("屏幕收到红外信号", "len", len(raw))
	f(raw)
}
