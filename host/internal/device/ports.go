package device

import (
	"errors"
	"io"
	"slices"
	"strings"

	"go.bug.st/serial"
)

// espressifVID is the USB vendor ID of Espressif's native USB
// (USB-Serial/JTAG reports 303A:1001, TinyUSB CDC 303A:4001).
const espressifVID = "303A"

// PortInfo describes one serial port for the web UI.
type PortInfo struct {
	Name      string `json:"name"`
	VID       string `json:"vid,omitempty"`
	PID       string `json:"pid,omitempty"`
	Serial    string `json:"serial,omitempty"`
	Product   string `json:"product,omitempty"`
	Candidate bool   `json:"candidate"`
}

// isDarwinDialIn reports macOS /dev/tty.* ports, whose /dev/cu.* twin is
// the one to open for outgoing connections.
func isDarwinDialIn(name string) bool { return strings.HasPrefix(name, "/dev/tty.") }

// pickPort chooses the best candidate: PIDs 1001/4001 first, then by name
// for a stable choice.
func pickPort(ports []PortInfo) string {
	var cands []PortInfo
	for _, p := range ports {
		if p.Candidate {
			cands = append(cands, p)
		}
	}
	rank := func(p PortInfo) int {
		switch p.PID {
		case "1001", "4001":
			return 0
		}
		return 1
	}
	slices.SortStableFunc(cands, func(a, b PortInfo) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		return strings.Compare(a.Name, b.Name)
	})
	if len(cands) == 0 {
		return ""
	}
	return cands[0].Name
}

// openSerial opens a port with DTR on and RTS off: on the ESP32-S3's
// USB-Serial/JTAG, toggling RTS can reset the chip, while TinyUSB CDC
// firmware uses DTR as "host connected".
func openSerial(name string) (io.ReadWriteCloser, error) {
	p, err := serial.Open(name, &serial.Mode{
		BaudRate:          115200,
		InitialStatusBits: &serial.ModemOutputBits{DTR: true, RTS: false},
	})
	if err == nil {
		return p, nil
	}
	// Ports without modem control lines (ptys, some bridges) reject the
	// DTR/RTS setup; they work without it.
	var pe *serial.PortError
	if errors.As(err, &pe) && pe.Code() == serial.InvalidSerialPort {
		if p, err2 := serial.Open(name, &serial.Mode{BaudRate: 115200}); err2 == nil {
			return p, nil
		}
	}
	return nil, err
}
