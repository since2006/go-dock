// Package logx wires slog to a rotating log file and an in-memory ring
// buffer that the web UI can read and stream.
package logx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Entry is one log record as shown in the web UI.
type Entry struct {
	Seq   uint64    `json:"seq"`
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
	Attrs string    `json:"attrs,omitempty"`
}

// Ring keeps the most recent entries and fans new ones out to subscribers.
type Ring struct {
	mu   sync.Mutex
	buf  []Entry
	next int // index the next entry goes to once buf is full
	seq  uint64
	subs map[chan Entry]struct{}
}

func NewRing(capacity int) *Ring {
	return &Ring{buf: make([]Entry, 0, capacity), subs: map[chan Entry]struct{}{}}
}

func (r *Ring) add(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	e.Seq = r.seq
	if len(r.buf) < cap(r.buf) {
		r.buf = append(r.buf, e)
	} else {
		r.buf[r.next] = e
		r.next = (r.next + 1) % len(r.buf)
	}
	for ch := range r.subs {
		select {
		case ch <- e:
		default: // slow reader; it can resync from Entries
		}
	}
}

// Entries returns up to limit entries at or above minLevel, oldest first.
func (r *Ring) Entries(minLevel slog.Level, limit int) []Entry {
	r.mu.Lock()
	ordered := make([]Entry, 0, len(r.buf))
	ordered = append(ordered, r.buf[r.next:]...)
	ordered = append(ordered, r.buf[:r.next]...)
	r.mu.Unlock()

	out := make([]Entry, 0, len(ordered))
	for _, e := range ordered {
		if ParseLevel(e.Level) >= minLevel {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Subscribe returns a channel of new entries and a cancel func.
func (r *Ring) Subscribe() (<-chan Entry, func()) {
	ch := make(chan Entry, 256)
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		delete(r.subs, ch)
		r.mu.Unlock()
	}
}

func ParseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}

// Logger bundles the configured slog.Logger with its level and ring.
type Logger struct {
	*slog.Logger
	Level *slog.LevelVar
	Ring  *Ring
	file  io.Closer
}

// New logs to path (rotated) and, if console is non-nil, also to console.
func New(path string, console io.Writer, level slog.Level) *Logger {
	lv := new(slog.LevelVar)
	lv.Set(level)
	ring := NewRing(2000)
	lj := &lumberjack.Logger{Filename: path, MaxSize: 10, MaxBackups: 3, MaxAge: 14}
	var w io.Writer = lj
	if console != nil {
		w = io.MultiWriter(lj, console)
	}
	text := slog.NewTextHandler(w, &slog.HandlerOptions{Level: lv})
	h := &handler{next: text, ring: ring, level: lv}
	return &Logger{Logger: slog.New(h), Level: lv, Ring: ring, file: lj}
}

func (l *Logger) Close() error { return l.file.Close() }

// handler forwards to next and records a flattened copy in the ring.
type handler struct {
	next   slog.Handler
	ring   *Ring
	level  *slog.LevelVar
	prefix string // flattened attrs from WithAttrs
	group  string
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level.Level() }

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(h.prefix)
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&b, h.group, a)
		return true
	})
	h.ring.add(Entry{Time: r.Time, Level: r.Level.String(), Msg: r.Message, Attrs: strings.TrimSpace(b.String())})
	return h.next.Handle(ctx, r)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var b strings.Builder
	b.WriteString(h.prefix)
	for _, a := range attrs {
		appendAttr(&b, h.group, a)
	}
	c := *h
	c.next = h.next.WithAttrs(attrs)
	c.prefix = b.String()
	return &c
}

func (h *handler) WithGroup(name string) slog.Handler {
	c := *h
	c.next = h.next.WithGroup(name)
	if c.group != "" {
		c.group += "."
	}
	c.group += name
	return &c
}

func appendAttr(b *strings.Builder, group string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	key := a.Key
	if group != "" {
		key = group + "." + key
	}
	if a.Value.Kind() == slog.KindGroup {
		for _, ga := range a.Value.Group() {
			appendAttr(b, key, ga)
		}
		return
	}
	v := a.Value.String()
	if t := strings.TrimSpace(v); strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		// JSON (an MQTT payload, say) reads better as is than quoted.
		var c bytes.Buffer
		if json.Compact(&c, []byte(t)) == nil {
			fmt.Fprintf(b, " %s=%s", key, c.String())
			return
		}
	}
	if strings.ContainsAny(v, " \t\"=") {
		v = fmt.Sprintf("%q", v)
	}
	fmt.Fprintf(b, " %s=%s", key, v)
}
