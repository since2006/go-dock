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
	m.opts.Beep = "chime"
	if err := m.Beep("rise"); err == nil || !strings.Contains(err.Error(), "未连接") {
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
	if err := m.Beep("rise"); err == nil || !strings.Contains(err.Error(), "不支持") {
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
	if err := m.Beep("rise"); err != nil {
		t.Fatal(err)
	}
	if got := notesOf(t, dev.next(t, "beep")); !slices.Equal(got, SoundByID("rise").Notes) {
		t.Fatalf("notes %v", got)
	}
}
