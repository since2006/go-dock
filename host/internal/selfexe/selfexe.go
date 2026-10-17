// Package selfexe locates the running go-dock executable.
package selfexe

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrTemporary means the program runs from a `go run` build directory, so
// its path will not exist after it exits.
var ErrTemporary = errors.New("当前是 go run 生成的临时程序，请先 go build 出固定位置的可执行文件")

// Path returns the absolute, symlink-resolved path of the running binary.
func Path() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if strings.Contains(exe, "go-build") || strings.HasPrefix(exe, os.TempDir()) {
		return exe, ErrTemporary
	}
	return exe, nil
}
