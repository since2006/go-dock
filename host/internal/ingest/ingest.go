// Package ingest forwards hook and statusline payloads from short-lived
// go-dock subprocesses to the running daemon.
package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// MaxBody bounds what a hook may forward.
const MaxBody = 4 << 20

// Post sends body to the daemon at addr. It gives up after timeout so the
// calling tool is never held up when the daemon is not running.
func Post(ctx context.Context, addr, provider, kind string, body []byte, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	url := fmt.Sprintf("http://%s/api/ingest/%s/%s", addr, provider, kind)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("daemon replied %s", resp.Status)
	}
	return nil
}
