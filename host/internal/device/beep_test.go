package device

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/since2006/go-dock/host/internal/config"
	"github.com/since2006/go-dock/host/internal/model"
)

func TestSounds(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range Sounds {
		if !protoID.MatchString(s.ID) || seen[s.ID] || s.Name == "" {
			t.Errorf("sound %q: bad or duplicate id, or no name", s.ID)
		}
		seen[s.ID] = true
		if n := len(s.Notes); n == 0 || n > maxNotes {
			t.Errorf("%s: %d notes", s.ID, n)
		}
		for _, n := range s.Notes {
			if hz, ms := n[0], n[1]; (hz != 0 && (hz < minHz || hz > maxHz)) || ms < minMs || ms > maxMs {
				t.Errorf("%s: note %v out of range", s.ID, n)
			}
		}
	}
	if !seen[DefaultSound] || config.Default().Device.BeepSound != DefaultSound {
		t.Fatalf("default sound %q missing or not the config default", DefaultSound)
	}
	if SoundByID("nope").ID != DefaultSound || SoundByID("rise").ID != "rise" {
		t.Fatal("SoundByID")
	}
	if got := string(EncodeBeep([]Note{{2000, 100}, {0, 50}})); got != `{"t":"beep","notes":[[2000,100],[0,50]]}`+"\n" {
		t.Fatalf("encoded %q", got)
	}
}

// The longest beep a user can write still fits one line.
func TestBeepFitsLine(t *testing.T) {
	notes := make([]Note, maxNotes)
	for i := range notes {
		notes[i] = Note{maxHz, maxMs}
	}
	if n := len(EncodeBeep(notes)); n == 0 || n > MaxLine {
		t.Errorf("beep of %d notes is %d bytes", maxNotes, n)
	}
}

func TestParseNotes(t *testing.T) {
	for text, want := range map[string][]Note{
		"E7 100, - 40, C8 300":        {{2637, 100}, {0, 40}, {4186, 300}},
		" a4 50 ；Bb5 60\nF#6 70ms，":   {{440, 50}, {932, 60}, {1480, 70}},
		"C4 10; 0 20; 2730 90; ♭":     nil, // the last entry is no note
		"g♯6 100, e♭7 100, 8000 1000": {{1661, 100}, {2489, 100}, {8000, 1000}},
	} {
		got, err := ParseNotes(text)
		if want == nil {
			if err == nil {
				t.Errorf("%q: accepted as %v", text, got)
			}
			continue
		}
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%q: %v, %v; want %v", text, got, err, want)
		}
	}
	for _, bad := range []string{
		"", " , ", "H7 100", "E7", "E7 100 200", "E7 5", "E7 2000", "C1 100", "9000 100", "E10 100",
		strings.Repeat("E7 50, ", maxNotes+1),
	} {
		if got, err := ParseNotes(bad); err == nil {
			t.Errorf("%q: accepted as %v", bad, got)
		}
	}
	if _, err := ParseNotes("E7 100, X7 100"); err == nil || !strings.Contains(err.Error(), "第 2 个音") {
		t.Errorf("error does not say which note: %v", err)
	}
	if got, _ := ParseNotes(strings.Repeat("E7 50, ", maxNotes)); len(got) != maxNotes {
		t.Errorf("%d notes", len(got))
	}
}

func TestWaitingTasks(t *testing.T) {
	s := model.Snapshot{Sessions: []model.Session{
		{ID: "claude:a", State: model.StateApproval, Since: t0},
		{ID: "codex:b", State: model.StateInput, Since: t0},
		{ID: "claude:c", State: model.StateRunning, Since: t0, Agents: []model.Agent{
			{ID: "x", State: model.StateApproval},
			{ID: "y", State: model.StateRunning},
		}},
		{ID: "claude:d", State: model.StateError, Since: t0},
		{ID: "claude:e", State: model.StateDone, Since: t0},
	}}
	first := waitingTasks(s)
	if len(first) != 3 {
		t.Fatalf("waiting %v", first)
	}
	// Asking again after an approval nobody saw is a new request.
	s.Sessions[0].Since = t0.Add(time.Second)
	again := waitingTasks(s)
	fresh := 0
	for k := range again {
		if !first[k] {
			fresh++
		}
	}
	if len(again) != 3 || fresh != 1 {
		t.Fatalf("again %v, fresh %d", again, fresh)
	}
}

func notesOf(t *testing.T, m map[string]any) []Note {
	t.Helper()
	var out []Note
	for _, n := range m["notes"].([]any) {
		p := n.([]any)
		out = append(out, Note{int(p[0].(float64)), int(p[1].(float64))})
	}
	return out
}

func TestManagerBeep(t *testing.T) {
	const (
		helloPlain = `{"t":"hello","proto":1,"fw":"0.1.0","w":600,"h":450,"max_line":4096}`
		helloBeep  = `{"t":"hello","proto":1,"fw":"0.2.0","w":600,"h":450,"max_line":4096,"beep":1}`
	)
	src := &fakeSource{snap: sampleSnapshot(), ch: make(chan struct{}, 1)} // claude:a needs approval
	m, devices := newTestManager(t, src, io.Discard)
	m.opts.BeepNotes = SoundByID("chime").Notes
	if err := m.Beep(SoundByID("rise").Notes); err == nil || !strings.Contains(err.Error(), "未连接") {
		t.Fatalf("beep with no device: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	// Firmware without a buzzer gets no beeps.
	dev := <-devices
	dev.send(t, helloPlain)
	waitFor(t, "ready", func() bool { return m.Status().State == StateReady })
	dev.none(t, "beep", 100*time.Millisecond)
	if err := m.Beep(SoundByID("rise").Notes); err == nil || !strings.Contains(err.Error(), "不支持") {
		t.Fatalf("beep on old firmware: %v", err)
	}

	// With one, the task already waiting beeps once after the handshake.
	m.Reconnect()
	dev = <-devices
	dev.send(t, helloBeep)
	if got := notesOf(t, dev.next(t, "beep")); !slices.Equal(got, SoundByID("chime").Notes) {
		t.Fatalf("notes %v", got)
	}
	if !m.Status().Beep {
		t.Fatal("status says no beep")
	}
	src.ch <- struct{}{}
	dev.next(t, "snapshot")
	dev.none(t, "beep", 100*time.Millisecond)

	// Replugging does not beep again for it.
	m.Reconnect()
	dev = <-devices
	dev.send(t, helloBeep)
	waitFor(t, "ready again", func() bool { return m.Status().State == StateReady })
	dev.none(t, "beep", 100*time.Millisecond)

	// A subagent asking, and the session asking again, beep.
	s := sampleSnapshot()
	s.Sessions[1].Agents = []model.Agent{{ID: "x", Type: "Explore", State: model.StateApproval}}
	src.set(s)
	dev.next(t, "beep")
	s.Sessions[0].Since = t0
	src.set(s)
	dev.next(t, "beep")

	// Turned off, a new question stays quiet ...
	m.SetOptions(Options{Enabled: true, Port: "auto", MaxSessions: 12})
	s.Sessions[2].State = model.StateInput
	src.set(s)
	dev.next(t, "snapshot")
	dev.none(t, "beep", 100*time.Millisecond)

	// ... but trying a sound still plays it.
	if err := m.Beep(SoundByID("rise").Notes); err != nil {
		t.Fatal(err)
	}
	if got := notesOf(t, dev.next(t, "beep")); !slices.Equal(got, SoundByID("rise").Notes) {
		t.Fatalf("notes %v", got)
	}
}

func TestActiveTasks(t *testing.T) {
	s := model.Snapshot{Sessions: []model.Session{
		{ID: "a", State: model.StateRunning},
		{ID: "b", State: model.StateApproval},
		{ID: "c", State: model.StateDone, Agents: []model.Agent{{ID: "x", State: model.StateRunning}}},
		{ID: "d", State: model.StateDone},
		{ID: "e", State: model.StateError},
	}}
	now, finished := activeTasks(s, nil)
	if len(now) != 3 || !now["a"] || !now["b"] || !now["c"] || finished {
		t.Fatalf("first %v %v", now, finished)
	}
	// a ends in an error, b is answered and runs: nothing finished yet.
	s.Sessions[0].State, s.Sessions[1].State = model.StateError, model.StateRunning
	if now, finished = activeTasks(s, now); finished || len(now) != 2 {
		t.Fatalf("error %v %v", now, finished)
	}
	// c's subagent is done: c finished.
	s.Sessions[2].Agents[0].State = model.StateDone
	if now, finished = activeTasks(s, now); !finished || now["c"] {
		t.Fatalf("subagent done %v %v", now, finished)
	}
}

func TestManagerDoneBeep(t *testing.T) {
	const helloBeep = `{"t":"hello","proto":1,"fw":"0.2.0","w":600,"h":450,"max_line":4096,"beep":1}`
	chime, rise := SoundByID("chime").Notes, SoundByID("rise").Notes
	src := &fakeSource{snap: sampleSnapshot(), ch: make(chan struct{}, 1)} // a: approval, b: running, c: done
	m, devices := newTestManager(t, src, io.Discard)
	m.opts.BeepNotes, m.opts.DoneNotes = chime, rise
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	// claude:c was done before go-dock saw it work: only a's question rings.
	dev := <-devices
	dev.send(t, helloBeep)
	if got := notesOf(t, dev.next(t, "beep")); !slices.Equal(got, chime) {
		t.Fatalf("first %v", got)
	}
	dev.none(t, "beep", 100*time.Millisecond)

	// codex:b finishes: the done sound, once.
	s := sampleSnapshot()
	s.Sessions[1].State = model.StateDone
	src.set(s)
	if got := notesOf(t, dev.next(t, "beep")); !slices.Equal(got, rise) {
		t.Fatalf("done %v", got)
	}
	src.ch <- struct{}{}
	dev.next(t, "snapshot")
	dev.none(t, "beep", 100*time.Millisecond)

	// A task that starts waiting rings its own sound even when another
	// finishes with it.
	s.Sessions = slices.Clone(s.Sessions) // the manager may still read the last one
	s.Sessions[0].State = model.StateDone
	s.Sessions[2].State, s.Sessions[2].Since = model.StateInput, t0
	src.set(s)
	if got := notesOf(t, dev.next(t, "beep")); !slices.Equal(got, chime) {
		t.Fatalf("waiting and done %v", got)
	}
	dev.none(t, "beep", 100*time.Millisecond)

	// With the done sound off, finishing is quiet.
	m.SetOptions(Options{Enabled: true, Port: "auto", MaxSessions: 12, BeepNotes: chime})
	s.Sessions = slices.Clone(s.Sessions)
	s.Sessions[2].State = model.StateDone
	src.set(s)
	dev.next(t, "snapshot")
	dev.none(t, "beep", 100*time.Millisecond)
}
