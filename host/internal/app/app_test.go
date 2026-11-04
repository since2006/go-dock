package app

import (
	"testing"

	"github.com/since2006/go-dock/host/internal/config"
	"github.com/since2006/go-dock/host/internal/device"
)

func TestDeviceOptions(t *testing.T) {
	c := config.Default()
	o := deviceOptions(c)
	if o.Addr != "" || o.Port != "auto" || o.Name == "" || !device.ValidHostID(o.HostID) || len(o.HostID) != 16 || o.OwnSettings {
		t.Fatalf("default options = %+v", o)
	}
	if again := deviceOptions(c); again.HostID != o.HostID {
		t.Fatal("host id changes between calls")
	}

	c.Device.Addr, c.Device.Code, c.Device.Name = " 192.168.1.50 ", "482916", " 书房 "
	o = deviceOptions(c)
	if o.Addr != "192.168.1.50" || o.Code != "482916" || o.Name != "书房" || o.OwnSettings {
		t.Fatalf("options = %+v", o)
	}

	// Brightness of the user's own goes to the screen on every connecting.
	c.Device.DimBrightness = 0
	if o = deviceOptions(c); !o.OwnSettings {
		t.Fatalf("own settings not noticed: %+v", o)
	}
}
