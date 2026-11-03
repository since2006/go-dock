package model

// Reading is one item of the line on the Agent page, above the usage: a
// plugin's reading, such as the CPU or the memory, with its icon, then a
// bar when it has a percent, else a short note.
type Reading struct {
	ID    string `json:"id"`
	Icon  string `json:"icon,omitempty"` // Icon* below; empty draws none
	Label string `json:"label"`
	Text  string `json:"text"`           // the reading: 37, 2150, 停转
	Unit  string `json:"unit,omitempty"` // after it, smaller: %, rpm, KB/s
	Sub   string `json:"sub,omitempty"`  // shown instead of a bar: ↑ 1.5M
	Pct   *int   `json:"pct,omitempty"`  // 0–100, a bar
	// Level (Level*) colours a reading that needs attention.
	Level string `json:"level,omitempty"`
}

// Levels of a reading.
const (
	LevelWarn = "warn" // amber
	LevelCrit = "crit" // red
)

// Icons the screen draws.
const (
	IconCPU    = "cpu"
	IconGPU    = "gpu"
	IconMemory = "memory"
	IconRotor  = "rotor" // a fan's blades
	IconNet    = "net"
	IconDisk   = "disk"
)
