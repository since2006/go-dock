package jsonl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func collect(t *testing.T, tl *Tailer, path string) []string {
	t.Helper()
	var got []string
	if err := tl.Read(path, func(l []byte) { got = append(got, string(l)) }); err != nil {
		t.Fatal(err)
	}
	return got
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestTailerIncremental(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	tl := NewTailer()

	appendFile(t, path, "{\"a\":1}\n{\"a\":2")
	if got := collect(t, tl, path); len(got) != 1 || got[0] != `{"a":1}` {
		t.Fatalf("first read = %v", got)
	}
	appendFile(t, path, "}\n\n{\"a\":3}\n")
	if got := collect(t, tl, path); len(got) != 2 || got[0] != `{"a":2}` || got[1] != `{"a":3}` {
		t.Fatalf("second read = %v", got)
	}
	if got := collect(t, tl, path); len(got) != 0 {
		t.Fatalf("unchanged file returned %v", got)
	}

	// Truncation restarts from the beginning.
	if err := os.WriteFile(path, []byte("{\"b\":1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Ensure the mtime differs on coarse filesystems.
	future := time.Now().Add(time.Second)
	os.Chtimes(path, future, future)
	if got := collect(t, tl, path); len(got) != 1 || got[0] != `{"b":1}` {
		t.Fatalf("after truncate = %v", got)
	}
}

func TestTailerLongLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "long.jsonl")
	tl := NewTailer()
	long := `{"x":"` + strings.Repeat("y", 600<<10) + `"}`
	appendFile(t, path, long)
	if got := collect(t, tl, path); len(got) != 0 {
		t.Fatalf("unfinished long line returned")
	}
	appendFile(t, path, "\n{\"n\":2}\n")
	got := collect(t, tl, path)
	if len(got) != 2 || got[0] != long || got[1] != `{"n":2}` {
		t.Fatalf("long line read wrong: %d lines", len(got))
	}
	appendFile(t, path, "{\"n\":3}\n")
	if got := collect(t, tl, path); len(got) != 1 || got[0] != `{"n":3}` {
		t.Fatalf("offset wrong after long line: %v", got)
	}
}

func TestWalk(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "x", "y"), 0o755)
	os.WriteFile(filepath.Join(root, "x", "y", "a.jsonl"), nil, 0o644)
	os.WriteFile(filepath.Join(root, "x", "b.txt"), nil, 0o644)
	old := filepath.Join(root, "old.jsonl")
	os.WriteFile(old, nil, 0o644)
	past := time.Now().Add(-72 * time.Hour)
	os.Chtimes(old, past, past)

	got := Walk(root, 48*time.Hour, func(p string) bool { return strings.HasSuffix(p, ".jsonl") })
	if len(got) != 1 || filepath.Base(got[0]) != "a.jsonl" {
		t.Fatalf("walk = %v", got)
	}
	if got := Walk(filepath.Join(root, "missing"), 0, func(string) bool { return true }); len(got) != 0 {
		t.Fatalf("missing root = %v", got)
	}
}
