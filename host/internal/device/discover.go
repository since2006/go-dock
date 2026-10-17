//go:build !darwin || cgo

package device

import (
	"strings"

	"go.bug.st/serial/enumerator"
)

// ListPorts lists serial ports with USB details. Ports with the Espressif
// vendor ID are candidates.
func ListPorts() ([]PortInfo, error) {
	list, err := enumerator.GetDetailedPortsList(func(vid, pid string) bool {
		return strings.EqualFold(vid, espressifVID)
	})
	if err != nil {
		return nil, err
	}
	out := make([]PortInfo, 0, len(list))
	for _, p := range list {
		info := PortInfo{
			Name:    p.Name,
			VID:     strings.ToUpper(p.VID),
			PID:     strings.ToUpper(p.PID),
			Serial:  p.SerialNumber,
			Product: p.Product,
		}
		info.Candidate = p.IsUSB && info.VID == espressifVID && !isDarwinDialIn(p.Name)
		out = append(out, info)
	}
	return out, nil
}
