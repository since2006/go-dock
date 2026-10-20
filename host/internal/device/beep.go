package device

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/since2006/go-dock/host/internal/model"
)

// Beeps (docs/protocol-v1.md §3.6): the firmware drives a passive buzzer
// wired to the board's pin header and plays the notes it is sent, so the
// sounds live here and a new one needs no firmware update.

const (
	maxNotes = 320 // the firmware's BEEP_MAX_NOTES; the longest beep must fit MaxLine
	minHz    = 100
	maxHz    = 8000
	minMs    = 10
	maxMs    = 1000
)

// Note is one tone, [Hz, ms]; 0 Hz is a rest.
type Note = [2]int

// Sound is an alert melody users can pick.
type Sound struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Notes []Note `json:"notes"`
}

// DefaultSound plays when the configured one is unknown.
const DefaultSound = "chime"

// CustomSound is the setting for notes the user writes (ParseNotes).
const CustomSound = "custom"

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
	// 8-bit: the buzzer's square wave sounds like the old game consoles;
	// the tunes are our own.
	{ID: "pickup", Name: "拾取", Notes: []Note{{2637, 50}, {3951, 220}}},
	{ID: "levelup", Name: "升级", Notes: []Note{
		{2093, 50}, {0, 15}, {2637, 50}, {0, 15}, {3136, 50}, {0, 15}, {4186, 60}, {3136, 50}, {4186, 240},
	}},
	{ID: "fanfare", Name: "过关", Notes: []Note{{1568, 90}, {2093, 90}, {2637, 90}, {3136, 180}, {2637, 90}, {3136, 360}}},
	{ID: "jump", Name: "跳跃", Notes: []Note{ // a quick slide up an octave
		{2349, 12}, {2489, 12}, {2637, 12}, {2794, 12}, {2960, 12}, {3136, 12}, {3322, 12},
		{3520, 12}, {3729, 12}, {3951, 12}, {4186, 12}, {4435, 12}, {4699, 60},
	}},
	{ID: "siren", Name: "警笛", Notes: []Note{{3520, 90}, {2349, 90}, {3520, 90}, {2349, 90}, {3520, 90}, {2349, 90}}},
}

// ParseNotes reads notes the user wrote for a custom sound: entries
// separated by commas (or semicolons, new lines), each a pitch and a length
// in ms. The pitch is a note name with its octave (C4 is middle C; F#5,
// Bb6), a frequency in Hz, or "-" for a rest: "E7 100, - 40, C8 300".
func ParseNotes(text string) ([]Note, error) {
	var notes []Note
	for i, entry := range strings.FieldsFunc(text, func(r rune) bool {
		return strings.ContainsRune(",，;；\n\r", r)
	}) {
		f := strings.Fields(entry)
		if len(f) == 0 {
			continue
		}
		if len(f) != 2 {
			return nil, fmt.Errorf("第 %d 个音「%s」要写成「音高 时长」，如 E7 100", i+1, strings.TrimSpace(entry))
		}
		hz, err := pitch(f[0])
		if err != nil {
			return nil, fmt.Errorf("第 %d 个音：%w", i+1, err)
		}
		ms, err := strconv.Atoi(strings.TrimSuffix(f[1], "ms"))
		if err != nil || ms < minMs || ms > maxMs {
			return nil, fmt.Errorf("第 %d 个音的时长「%s」要在 %d–%d 毫秒之间", i+1, f[1], minMs, maxMs)
		}
		notes = append(notes, Note{hz, ms})
	}
	switch {
	case len(notes) == 0:
		return nil, errors.New("还没有填音符")
	case len(notes) > maxNotes:
		return nil, fmt.Errorf("最多 %d 个音，现在有 %d 个", maxNotes, len(notes))
	}
	return notes, nil
}

// pitch reads "-" or "0" (a rest), a frequency, or a note name with its
// octave, in equal temperament from A4 = 440 Hz.
func pitch(s string) (int, error) {
	if s == "-" || s == "0" {
		return 0, nil
	}
	hz, err := strconv.Atoi(s)
	if err != nil {
		// A letter, then # or b (♯, ♭), then the octave.
		semis := map[rune]int{'C': -9, 'D': -7, 'E': -5, 'F': -4, 'G': -2, 'A': 0, 'B': 2}
		r := []rune(s)
		n, ok := semis[unicode.ToUpper(r[0])]
		if len(r) > 1 {
			switch r[1] {
			case '#', '♯':
				n, r = n+1, r[1:]
			case 'b', '♭':
				n, r = n-1, r[1:]
			}
		}
		octave, err := strconv.Atoi(string(r[1:]))
		if !ok || err != nil || octave < 0 || octave > 9 {
			return 0, fmt.Errorf("不认识的音高「%s」，写音名加八度（如 E7、F#6、Bb5）或频率（Hz）", s)
		}
		hz = int(math.Round(440 * math.Pow(2, float64(n+12*(octave-4))/12)))
	}
	if hz < minHz || hz > maxHz {
		return 0, fmt.Errorf("音高「%s」是 %d Hz，要在 %d–%d Hz 之间", s, hz, minHz, maxHz)
	}
	return hz, nil
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
	// Vol is the volume in percent, left out for full volume. Firmware that
	// announces "beep":2 plays it; older firmware plays everything at 100.
	Vol int `json:"vol,omitempty"`
}

// EncodeBeep encodes the beep message for notes at a volume in percent;
// 100 or more plays at full volume.
func EncodeBeep(notes []Note, vol int) []byte {
	b := Beep{T: "beep", Notes: notes}
	if vol < 100 {
		b.Vol = max(vol, 1)
	}
	line, _ := encodeLine(b)
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

// activeTasks lists the sessions working or waiting for the user (their
// subagents included), and reports whether one listed in was has finished
// since. A session that ends in an error, or was already done when first
// seen, has not just finished.
func activeTasks(s model.Snapshot, was map[string]bool) (map[string]bool, bool) {
	now, finished := map[string]bool{}, false
	for _, ss := range s.Sessions {
		switch st, _ := ss.Effective(); st {
		case model.StateRunning, model.StateApproval, model.StateInput:
			now[ss.ID] = true
		case model.StateDone:
			finished = finished || was[ss.ID]
		}
	}
	return now, finished
}
