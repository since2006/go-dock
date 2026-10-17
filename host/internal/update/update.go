// Package update checks GitHub Releases for a newer go-dock and replaces
// the running executable with it.
//
// A release carries one raw binary per platform, named by AssetName, and a
// SHA256SUMS file. The download is verified against SHA256SUMS and by
// running "<new binary> version" before it replaces the current one.
package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/since2006/go-dock/host/internal/selfexe"
)

// DefaultAPI is the GitHub API base of the go-dock repository. The
// GO_DOCK_UPDATE_API environment variable overrides it for testing.
const DefaultAPI = "https://api.github.com/repos/since2006/go-dock"

const (
	maxBinary   = 200 << 20
	maxNotes    = 4000
	checkPeriod = 24 * time.Hour
)

// AssetName is the release file holding the binary for a platform.
func AssetName(goos, goarch string) string {
	name := "go-dock_" + goos + "_" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// Status is what the web UI shows about updates.
type Status struct {
	Current   string    `json:"current"`
	Dev       bool      `json:"dev"` // development build: cannot update itself
	Latest    string    `json:"latest,omitempty"`
	Available bool      `json:"available"`
	Notes     string    `json:"notes,omitempty"`
	URL       string    `json:"url,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitzero"`
	// State is idle, checking, downloading, restarting or error.
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

type release struct {
	Tag    string `json:"tag_name"`
	Body   string `json:"body"`
	URL    string `json:"html_url"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (r *release) asset(name string) string {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL
		}
	}
	return ""
}

type Updater struct {
	current  string
	api      string
	client   *http.Client
	log      *slog.Logger
	onChange func()

	// Replaced in tests.
	exe    func() (string, error)
	verify func(ctx context.Context, path, version string) error
	goos   string
	goarch string

	mu   sync.Mutex
	st   Status
	rel  *release
	busy bool
}

// New creates an updater for the running version. onChange is called
// whenever Status changes.
func New(current string, log *slog.Logger, onChange func()) *Updater {
	api := DefaultAPI
	if v := os.Getenv("GO_DOCK_UPDATE_API"); v != "" {
		api = strings.TrimRight(v, "/")
	}
	_, ok := parseVersion(current)
	if onChange == nil {
		onChange = func() {}
	}
	return &Updater{
		current:  current,
		api:      api,
		client:   &http.Client{Timeout: 5 * time.Minute},
		log:      log.With("component", "update"),
		onChange: onChange,
		exe:      selfexe.Path,
		verify:   runVersion,
		goos:     runtime.GOOS,
		goarch:   runtime.GOARCH,
		st:       Status{Current: current, Dev: !ok, State: "idle"},
	}
}

func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.st
}

func (u *Updater) set(f func(*Status)) {
	u.mu.Lock()
	f(&u.st)
	u.mu.Unlock()
	u.onChange()
}

// Run checks once shortly after start and then daily while autoCheck
// returns true.
func (u *Updater) Run(ctx context.Context, autoCheck func() bool) {
	t := time.NewTimer(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		t.Reset(time.Hour)
		last := u.Status().CheckedAt
		if autoCheck() && (last.IsZero() || time.Since(last) >= checkPeriod) {
			if err := u.Check(ctx); err != nil {
				u.log.Debug("检查更新失败", "err", err)
			}
		}
	}
}

// Check asks GitHub for the latest release.
func (u *Updater) Check(ctx context.Context) error {
	u.mu.Lock()
	if u.busy {
		u.mu.Unlock()
		return errors.New("正在升级，请稍候")
	}
	u.busy = true
	u.mu.Unlock()
	defer func() { u.mu.Lock(); u.busy = false; u.mu.Unlock() }()

	u.set(func(s *Status) { s.State, s.Error = "checking", "" })
	rel, err := u.latest(ctx)
	if err != nil {
		u.set(func(s *Status) { s.State, s.Error, s.CheckedAt = "error", err.Error(), time.Now() })
		return err
	}
	u.mu.Lock()
	u.rel = rel
	u.mu.Unlock()
	u.set(func(s *Status) {
		s.State, s.CheckedAt = "idle", time.Now()
		if rel == nil {
			s.Latest, s.Available, s.Notes, s.URL = "", false, "", ""
			return
		}
		s.Latest = strings.TrimPrefix(rel.Tag, "v")
		s.Available = Newer(s.Latest, s.Current)
		s.Notes = truncate(rel.Body, maxNotes)
		s.URL = rel.URL
	})
	st := u.Status()
	if st.Available {
		u.log.Info("发现新版本", "current", st.Current, "latest", st.Latest)
	}
	return nil
}

// latest returns the newest published release, or nil when there is none.
func (u *Updater) latest(ctx context.Context) (*release, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.api+"/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "go-dock/"+u.current)
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接 GitHub 失败: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, nil
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return nil, errors.New("GitHub 接口请求过于频繁，请稍后再试")
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub 返回 %s", resp.Status)
	}
	var rel release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析版本信息: %w", err)
	}
	return &rel, nil
}

// Apply downloads the latest release, verifies it and replaces the running
// executable. The caller restarts the program when it returns nil.
func (u *Updater) Apply(ctx context.Context) (err error) {
	u.mu.Lock()
	switch {
	case u.busy:
		u.mu.Unlock()
		return errors.New("正在检查或升级，请稍候")
	case u.st.Dev:
		u.mu.Unlock()
		return errors.New("开发版本不支持在线升级，请下载发布版本")
	case u.rel == nil || !u.st.Available:
		u.mu.Unlock()
		return errors.New("没有可用的新版本")
	}
	u.busy = true
	rel, version := u.rel, u.st.Latest
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.busy = false
		u.mu.Unlock()
		if err != nil {
			u.log.Warn("升级失败", "err", err)
			u.set(func(s *Status) { s.State, s.Error = "error", err.Error() })
		}
	}()

	u.set(func(s *Status) { s.State, s.Error = "downloading", "" })
	exe, err := u.exe()
	if err != nil {
		return err
	}
	name := AssetName(u.goos, u.goarch)
	binURL, sumsURL := rel.asset(name), rel.asset("SHA256SUMS")
	if binURL == "" || sumsURL == "" {
		return fmt.Errorf("版本 %s 没有提供 %s", version, name)
	}
	want, err := u.checksum(ctx, sumsURL, name)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(exe), ".go-dock-update-*")
	if err != nil {
		return fmt.Errorf("无法写入程序所在目录 %s，请手动替换: %w", filepath.Dir(exe), err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once renamed into place
	got, err := u.download(ctx, binURL, tmp)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("校验失败：%s 的 SHA-256 与 SHA256SUMS 不一致", name)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	if err := u.verify(ctx, tmpPath, version); err != nil {
		return fmt.Errorf("新版本无法运行: %w", err)
	}
	if err := replace(exe, tmpPath); err != nil {
		return fmt.Errorf("替换程序失败: %w", err)
	}
	u.log.Info("已升级，即将重启", "from", u.current, "to", version)
	u.set(func(s *Status) { s.State = "restarting" })
	return nil
}

func (u *Updater) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "go-dock/"+u.current)
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("下载失败: %s", resp.Status)
	}
	return resp, nil
}

// checksum finds name in the SHA256SUMS file at url.
func (u *Updater) checksum(ctx context.Context, url, name string) (string, error) {
	resp, err := u.get(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS 中没有 %s", name)
}

// download writes url to w and returns the hex SHA-256 of the content.
func (u *Updater) download(ctx context.Context, url string, w io.Writer) (string, error) {
	resp, err := u.get(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, maxBinary+1))
	if err != nil {
		return "", fmt.Errorf("下载失败: %w", err)
	}
	if n > maxBinary {
		return "", errors.New("下载的文件过大")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// runVersion checks that the new binary starts and reports the expected
// version.
func runVersion(ctx context.Context, path, version string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(out)); got != version {
		return fmt.Errorf("版本号为 %q，应为 %q", got, version)
	}
	return nil
}

// replace moves next over exe. Windows cannot overwrite a running
// executable but can rename it, so the old file is moved aside first and
// removed by Cleanup on the next start.
func replace(exe, next string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(next, exe)
	}
	old := exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(next, exe); err != nil {
		_ = os.Rename(old, exe)
		return err
	}
	return nil
}

// Cleanup removes what a previous Windows update left behind.
func Cleanup() {
	if exe, err := selfexe.Path(); err == nil {
		_ = os.Remove(exe + ".old")
	}
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// parseVersion reads "1.2.3" or "v1.2.3", optionally with a "-pre" suffix.
func parseVersion(s string) (v [4]int, ok bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n := 0
		if p == "" {
			return v, false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return v, false
			}
			n = n*10 + int(c-'0')
		}
		v[i] = n
	}
	// A release sorts after its pre-releases.
	v[3] = 1
	if hasPre && pre != "" {
		v[3] = 0
	}
	return v, true
}

// Newer reports whether version a is newer than b. Unparseable versions
// are never newer.
func Newer(a, b string) bool {
	va, ok := parseVersion(a)
	if !ok {
		return false
	}
	vb, ok := parseVersion(b)
	if !ok {
		return true
	}
	for i := range va {
		if va[i] != vb[i] {
			return va[i] > vb[i]
		}
	}
	return false
}
