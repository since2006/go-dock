package device

import (
	"io"
	"time"
)

// Screen brightness (docs/protocol-v1.md §3.3): go-dock keeps the setting,
// the screen decides when to dim (tasks, touch, the link going away) and
// keeps the last values it got.

// Settings is the settings message.
type Settings struct {
	T             string `json:"t"`
	Brightness    int    `json:"brightness"`     // percent, 10-100
	DimBrightness int    `json:"dim_brightness"` // percent, 0 turns the screen off
	DimAfter      int    `json:"dim_after"`      // seconds without an active task; 0 never dims
}

// EncodeSettings encodes the brightness options as a settings message.
func EncodeSettings(o Options) []byte {
	line, _ := encodeLine(Settings{
		T:             "settings",
		Brightness:    o.Brightness,
		DimBrightness: o.DimBrightness,
		DimAfter:      int(o.DimAfter / time.Second),
	})
	return line
}

// sendSettings sends the brightness settings: always when they just
// changed, on connecting only when they are the user's own or changed
// while the screen was away. The screen keeps what it got last, from
// whichever computer.
func (m *Manager) sendSettings(conn io.WriteCloser, changed bool) error {
	m.mu.Lock()
	o, due := m.opts, m.settingsDue
	m.mu.Unlock()
	if !changed && !due && !o.OwnSettings {
		return nil
	}
	if err := m.write(conn, EncodeSettings(o)); err != nil {
		return err
	}
	m.mu.Lock()
	if sameSettings(m.opts, o) {
		m.settingsDue = false
	}
	m.mu.Unlock()
	return nil
}

// sameSettings reports whether a and b send the screen the same settings.
func sameSettings(a, b Options) bool {
	return a.Brightness == b.Brightness && a.DimBrightness == b.DimBrightness && a.DimAfter == b.DimAfter
}
