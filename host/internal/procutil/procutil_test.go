package procutil

import (
	"os"
	"testing"
)

func TestAlive(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Fatal("own process reported dead")
	}
	if Alive(0) || Alive(-1) {
		t.Fatal("invalid pid reported alive")
	}
}
