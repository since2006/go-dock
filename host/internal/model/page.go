package model

// Page is a function page a plugin shows next to the Agent page: a title
// and a few generic widgets the screen knows how to draw.
type Page struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`
	Error   string `json:"error,omitempty"` // why the page cannot be used right now
	// Movable: a card held down can be dragged onto another, and the two
	// trade places (ActSwap).
	Movable bool       `json:"movable,omitempty"`
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
	// A meter's reading: Text with Unit after it in a smaller font, Sub a
	// short note ("52°C"), Pct (0–100) a bar, Hist the last readings
	// scaled to 0–100, oldest first, drawn as a line instead of the bar;
	// Level (Level*) colours a reading that needs attention.
	Unit  string `json:"unit,omitempty"`
	Sub   string `json:"sub,omitempty"`
	Pct   *int   `json:"pct,omitempty"`
	Hist  []int  `json:"hist,omitempty"`
	Level string `json:"level,omitempty"`
	// A big meter takes the top row of its page, the other items share the
	// row below. It has a line under its label (Note on the left, Sub on
	// the right), a chart of Series instead of Hist, and Rows naming them.
	Big    bool     `json:"big,omitempty"`
	Note   string   `json:"note,omitempty"`
	Series []Series `json:"series,omitempty"`
	Rows   []Row    `json:"rows,omitempty"`
}

// Series is one area of a big meter's chart: readings scaled to 0–100,
// oldest first. The first series is drawn in front, so a stack sends
// running totals (user, then user + system).
type Series struct {
	Color string `json:"c"`
	Hist  []int  `json:"hist"`
}

// Row is one line of a big meter's legend: a swatch, a name and a value.
type Row struct {
	Label string `json:"label"`
	Text  string `json:"text"`
	Color string `json:"c"`
}

// Colours a meter's series and rows name.
const (
	ColorBlue   = "blue"
	ColorRed    = "red"
	ColorTeal   = "teal"
	ColorGreen  = "green"
	ColorOrange = "orange"
	ColorPurple = "purple"
	ColorGray   = "gray"
)

// Item kinds.
const (
	KindSwitch  = "switch"  // on or off: on, off, toggle (on and off with Buttons)
	KindStepper = "stepper" // a value with − and +: dec, inc
	KindSelect  = "select"  // one of a few options, a tap takes the next: next
	KindMeter   = "meter"   // a reading, with a line or a bar; takes no taps
)

// Meter levels.
const (
	LevelWarn = "warn" // amber
	LevelCrit = "crit" // red
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
	// The 性能 page.
	IconCPU    = "cpu"
	IconGPU    = "gpu"
	IconMemory = "memory"
	IconRotor  = "rotor" // a fan's blades
	IconNet    = "net"
	IconDisk   = "disk"
)

// Action is a tap on a page item, from the screen or the web UI. To is
// the item another was dropped on (ActSwap).
type Action struct {
	Page string `json:"page"`
	Item string `json:"item"`
	Act  string `json:"act"`
	To   string `json:"to,omitempty"`
}

// Acts the kinds take.
const (
	ActOn     = "on"
	ActOff    = "off"
	ActToggle = "toggle"
	ActInc    = "inc"
	ActDec    = "dec"
	ActNext   = "next"
	ActSwap   = "swap" // Item was dropped on To on a movable page
)
