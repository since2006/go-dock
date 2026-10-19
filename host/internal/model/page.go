package model

// Page is a function page a plugin shows next to the Agent page: a title
// and a few generic widgets the screen knows how to draw.
type Page struct {
	ID      string     `json:"id"`
	Title   string     `json:"title"`
	Summary string     `json:"summary,omitempty"`
	Error   string     `json:"error,omitempty"` // why the page cannot be used right now
	Items   []PageItem `json:"items"`
}

// PageItem is one widget on a page. Kind selects how it is drawn and which
// acts it takes.
type PageItem struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Text  string `json:"text"`
	// Icon names the picture on the card (Icon* below); empty is a bulb.
	Icon string `json:"icon,omitempty"`
	// On is a switch's state; nil means unknown.
	On *bool `json:"on,omitempty"`
	// Buttons: a switch shows an on and an off button instead of toggling
	// on a tap, for things whose state cannot be trusted.
	Buttons  bool `json:"buttons,omitempty"`
	Busy     bool `json:"busy,omitempty"` // a command is on its way
	Disabled bool `json:"disabled,omitempty"`
	// AtMin and AtMax: a stepper's value cannot go lower or higher.
	AtMin bool `json:"at_min,omitempty"`
	AtMax bool `json:"at_max,omitempty"`
}

// Item kinds.
const (
	KindSwitch  = "switch"  // on or off: on, off, toggle (on and off with Buttons)
	KindStepper = "stepper" // a value with − and +: dec, inc
	KindSelect  = "select"  // one of a few options, a tap takes the next: next
)

// Icons the screen draws.
const (
	IconBulb  = "bulb"
	IconPower = "power"
	IconCool  = "cool"
	IconHeat  = "heat"
	IconDry   = "dry"
	IconFan   = "fan"
	IconAuto  = "auto"
	IconMoon  = "moon"
	IconBell  = "bell"
)

// Action is a tap on a page item, from the screen or the web UI.
type Action struct {
	Page string `json:"page"`
	Item string `json:"item"`
	Act  string `json:"act"`
}

// Acts the kinds take.
const (
	ActOn     = "on"
	ActOff    = "off"
	ActToggle = "toggle"
	ActInc    = "inc"
	ActDec    = "dec"
	ActNext   = "next"
)
