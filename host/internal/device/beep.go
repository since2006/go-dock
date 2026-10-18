package device

import (
	"fmt"

	"github.com/since2006/go-dock/host/internal/model"
)

// Beeps (docs/protocol-v1.md §3.6): the firmware drives a passive buzzer
// wired to the board's pin header and plays the notes it is sent, so the
// sounds live here and a new one needs no firmware update.

const (
	maxNotes = 16 // the firmware's BEEP_MAX_NOTES
	minHz    = 100
	maxHz    = 8000
	minMs    = 10
	maxMs    = 1000
)

// Note is one tone, [Hz, ms]; 0 Hz is a rest.
type Note [2]int

// Sound is an alert melody users can pick.
type Sound struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Notes []Note `json:"notes"`
}

// DefaultSound plays when the configured one is unknown.
const DefaultSound = "chime"

// Sounds lists the alert melodies in the order the web UI offers them.
// Passive buzzers are loudest around 2–4 kHz, so the notes stay there.
var Sounds = []Sound{
	{ID: "chime", Name: "叮咚", Notes: []Note{{2637, 160}, {0, 40}, {2093, 320}}},
	{ID: "beep", Name: "嘀嘀", Notes: []Note{{2730, 90}, {0, 70}, {2730, 90}}},
	{ID: "rise", Name: "上扬", Notes: []Note{{2093, 100}, {2637, 100}, {3136, 220}}},
	{ID: "alert", Name: "急促", Notes: []Note{
		{3136, 60}, {0, 50}, {3136, 60}, {0, 50}, {3136, 60}, {0, 250},
		{3136, 60}, {0, 50}, {3136, 60}, {0, 50}, {3136, 60},
	}},
}

// SoundByID returns the sound with id, or the default one.
func SoundByID(id string) Sound {
	for _, s := range Sounds {
		if s.ID == id {
			return s
		}
	}
	return SoundByID(DefaultSound)
}

// Beep makes the device play notes now, cutting off what it plays.
type Beep struct {
	T     string `json:"t"`
	Notes []Note `json:"notes"`
}

// EncodeBeep encodes the beep message for notes.
func EncodeBeep(notes []Note) []byte {
	line, _ := encodeLine(Beep{T: "beep", Notes: notes})
	return line
}

// waitingTasks names the sessions and subagents waiting for the user, for
// approval or an answer. A session's name includes when it started
// waiting, so a new request right after an approval counts as new even if
// no snapshot saw it running in between.
func waitingTasks(s model.Snapshot) map[string]bool {
	keys := map[string]bool{}
	for _, ss := range s.Sessions {
		if needsUser(ss.State) {
			keys[fmt.Sprintf("%s@%d", ss.ID, ss.Since.UnixNano())] = true
		}
		for _, a := range ss.Agents {
			if needsUser(a.State) {
				keys[ss.ID+"/"+a.ID+"/"+string(a.State)] = true
			}
		}
	}
	return keys
}

func needsUser(s model.State) bool { return s == model.StateApproval || s == model.StateInput }
