package app

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// computerName is the name in System Settings → General → Sharing, such
// as "User 的 Mac mini"; the hostname is a made-up form of it.
func computerName() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "scutil", "--get", "ComputerName").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
