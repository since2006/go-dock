package logx

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

func TestRingAndHandler(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "x.log"), nil, slog.LevelInfo)
	defer l.Close()
	ch, cancel := l.Ring.Subscribe()
	defer cancel()

	l.Debug("hidden")
	l.With("provider", "claude").WithGroup("g").Info("hello", "k", "a b", "n", 3)
	l.Warn("careful")

	got := l.Ring.Entries(slog.LevelDebug, 0)
	if len(got) != 2 {
		t.Fatalf("entries = %+v", got)
	}
	if got[0].Msg != "hello" || !strings.Contains(got[0].Attrs, "provider=claude") || !strings.Contains(got[0].Attrs, `g.k="a b"`) {
		t.Fatalf("attrs = %q", got[0].Attrs)
	}
	if e := <-ch; e.Msg != "hello" {
		t.Fatalf("subscriber got %+v", e)
	}
	if w := l.Ring.Entries(slog.LevelWarn, 0); len(w) != 1 || w[0].Msg != "careful" {
		t.Fatalf("warn filter = %+v", w)
	}
}

func TestJSONAttrsShownAsIs(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "x.log"), nil, slog.LevelInfo)
	defer l.Close()
	l.Info("m", "payload", `{"state": "ON", "color": {"x": 0.3}}`, "list", "[1, 2]", "text", `[not json "x"`)
	got := l.Ring.Entries(slog.LevelDebug, 0)[0].Attrs
	if want := `payload={"state":"ON","color":{"x":0.3}} list=[1,2] text="[not json \"x\""`; got != want {
		t.Fatalf("attrs = %s\nwant    %s", got, want)
	}
}

func TestRingWraps(t *testing.T) {
	r := NewRing(3)
	for i := 0; i < 5; i++ {
		r.add(Entry{Level: "INFO", Msg: string(rune('a' + i))})
	}
	got := r.Entries(slog.LevelDebug, 0)
	if len(got) != 3 || got[0].Msg != "c" || got[2].Msg != "e" || got[2].Seq != 5 {
		t.Fatalf("wrapped = %+v", got)
	}
	if got := r.Entries(slog.LevelDebug, 2); len(got) != 2 || got[0].Msg != "d" {
		t.Fatalf("limit = %+v", got)
	}
}
