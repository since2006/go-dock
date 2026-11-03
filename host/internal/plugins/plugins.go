// Package plugins runs what go-dock shows on the screen besides the
// sessions: the line of readings on the Agent page, above the usage. A
// plugin keeps its own state and says what to show; the screen and the web
// preview only draw it.
//
// Adding one: implement Plugin in internal/plugins/<name>, give it a
// config section, and register it in internal/app.
package plugins

import "github.com/since2006/go-dock/host/internal/model"

// Plugin adds readings to the line.
type Plugin interface {
	// Readings are what the plugin shows now, none while it is off. It is
	// called from the device link and must not block.
	Readings() []model.Reading
}

// Host holds the plugins in line order.
type Host struct {
	plugins []Plugin
}

func New(ps ...Plugin) *Host {
	return &Host{plugins: ps}
}

// Line is every plugin's readings, in order.
func (h *Host) Line() []model.Reading {
	line := []model.Reading{}
	for _, p := range h.plugins {
		line = append(line, p.Readings()...)
	}
	return line
}
