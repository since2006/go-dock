package device

import (
	"errors"
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

// sameSettings reports whether a and b send the screen the same settings.
func sameSettings(a, b Options) bool {
	return a.Brightness == b.Brightness && a.DimBrightness == b.DimBrightness && a.DimAfter == b.DimAfter
}

// SettingsReady says why the screen cannot take brightness settings, or
// nil. A screen that is not connected gets them when it connects.
func (m *Manager) SettingsReady() error {
	if st := m.Status(); st.State == StateReady && !st.Settings {
		return errors.New("屏幕固件不支持调亮度，请先升级固件")
	}
	return nil
}
