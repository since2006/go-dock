package app

import (
	"testing"

	"github.com/since2006/go-dock/host/internal/device"
)

// The update goes over the cable when it is plugged in, else over WiFi,
// confirmed on the screen; a screen on neither link gets none.
func TestUpgradeRoute(t *testing.T) {
	ready := device.StateReady
	usb := device.USBStatus{State: ready, Firmware: "0.1.0"}
	cases := []struct {
		name    string
		st      device.Status
		local   bool
		upgrade bool
		confirm bool
	}{
		{"usb", device.Status{USB: usb}, false, true, false},
		{"usb beats wifi", device.Status{State: ready, Firmware: "0.1.0", USB: usb}, false, true, false},
		{"wifi", device.Status{State: ready, Firmware: "0.1.0"}, false, true, true},
		{"wifi, another protocol", device.Status{State: device.StateConnected, Firmware: "9.0.0", Incompatible: true}, false, true, true},
		{"up to date", device.Status{State: ready, Firmware: "0.2.0"}, false, false, false},
		{"newer on the screen", device.Status{State: ready, Firmware: "0.3.0"}, false, false, false},
		{"a local image goes on anything", device.Status{State: ready, Firmware: "0.3.0"}, true, true, true},
	}
	for _, c := range cases {
		st := deviceStatus{Status: c.st, Target: "0.2.0"}
		upgradeRoute(&st, c.local)
		if st.Upgradable != c.upgrade || st.Confirm != c.confirm {
			t.Errorf("%s: upgradable %v confirm %v, want %v %v", c.name, st.Upgradable, st.Confirm, c.upgrade, c.confirm)
		}
	}
}
