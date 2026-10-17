//go:build darwin && !cgo

package device

import (
	"strings"

	"go.bug.st/serial"
)

// ListPorts lists serial ports. Without cgo macOS cannot read USB IDs, so
// every /dev/cu.usbmodem* port (native USB CDC) is a candidate.
func ListPorts() ([]PortInfo, error) {
	names, err := serial.GetPortsList()
	if err != nil {
		return nil, err
	}
	out := make([]PortInfo, 0, len(names))
	for _, n := range names {
		out = append(out, PortInfo{Name: n, Candidate: strings.HasPrefix(n, "/dev/cu.usbmodem")})
	}
	return out, nil
}
