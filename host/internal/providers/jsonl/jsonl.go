// Package jsonl reads append-only JSON Lines files incrementally.
package jsonl

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// MaxLine bounds a single line; longer lines are skipped.
const MaxLine = 64 << 20

// Tailer remembers how far each file has been read and hands out only
// complete new lines. A partial last line is re-read once it is finished.
type Tailer struct {
	files map[string]*fileState
}

type fileState struct {
	offset int64
	size   int64
	mtime  time.Time
}

func NewTailer() *Tailer { return &Tailer{files: map[string]*fileState{}} }

// Known reports whether path has been read before.
func (t *Tailer) Known(path string) bool {
	_, ok := t.files[path]
	return ok
}

// Forget drops the offset for path, e.g. after the file disappeared.
func (t *Tailer) Forget(path string) { delete(t.files, path) }

// Paths returns every tracked path.
func (t *Tailer) Paths() []string {
	out := make([]string, 0, len(t.files))
	for p := range t.files {
		out = append(out, p)
	}
	return out
}

// Read calls fn for each complete line appended to path since the last
// call. It returns quickly when the file has not changed. If the file
// shrank, it is read again from the start. line is only valid during fn.
func (t *Tailer) Read(path string, fn func(line []byte)) error {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			t.Forget(path)
		}
		return err
	}
	st := t.files[path]
	if st == nil {
		st = &fileState{}
		t.files[path] = st
	}
	if fi.Size() == st.size && fi.ModTime().Equal(st.mtime) {
		return nil
	}
	if fi.Size() < st.offset {
		st.offset = 0
	}
	st.size, st.mtime = fi.Size(), fi.ModTime()

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(st.offset, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReaderSize(f, 256<<10)
	var long []byte // accumulates a line longer than the buffer
	skipping := false
	for {
		chunk, err := r.ReadSlice('\n')
		switch {
		case err == nil:
			st.offset += int64(len(long) + len(chunk))
			if skipping {
				skipping = false
				continue
			}
			line := chunk
			if long != nil {
				line = append(long, chunk...)
				long = nil
			}
			line = bytes.TrimSpace(line)
			if len(line) > 0 {
				fn(line)
			}
		case errors.Is(err, bufio.ErrBufferFull):
			// Part of a long line; offset only advances once it is complete.
			if skipping {
				st.offset += int64(len(chunk))
				continue
			}
			if len(long)+len(chunk) > MaxLine {
				st.offset += int64(len(long) + len(chunk))
				long, skipping = nil, true
				continue
			}
			long = append(long, chunk...)
		case errors.Is(err, io.EOF):
			if skipping {
				st.offset += int64(len(chunk))
			}
			return nil // unfinished line stays for next time
		default:
			return err
		}
	}
}

// Walk returns files under root accepted by match and modified within
// maxAge (0 means any age). A missing root yields no files.
func Walk(root string, maxAge time.Duration, match func(path string) bool) []string {
	var out []string
	cutoff := time.Now().Add(-maxAge)
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !match(path) {
			return nil
		}
		if maxAge > 0 {
			fi, err := d.Info()
			if err != nil || fi.ModTime().Before(cutoff) {
				return nil
			}
		}
		out = append(out, path)
		return nil
	})
	return out
}
