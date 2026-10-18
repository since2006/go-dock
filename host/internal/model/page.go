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
	// On is a switch's state; nil means unknown.
	On       *bool `json:"on,omitempty"`
	Busy     bool  `json:"busy,omitempty"` // a command is on its way
	Disabled bool  `json:"disabled,omitempty"`
}

// Item kinds.
const (
	KindSwitch = "switch"
)

// Action is a tap on a page item, from the screen or the web UI.
type Action struct {
	Page string `json:"page"`
	Item string `json:"item"`
	Act  string `json:"act"`
}

// Acts a switch takes.
const (
	ActOn     = "on"
	ActOff    = "off"
	ActToggle = "toggle"
)
