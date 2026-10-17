package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.2.0", "0.1.9", true},
		{"v1.0.0", "0.9.9", true},
		{"0.1.10", "0.1.9", true},
		{"0.1.0", "0.1.0", false},
		{"0.1.0", "0.2.0", false},
		{"0.2.0", "0.2.0-rc1", true},
		{"0.2.0-rc1", "0.1.0", true},
		{"0.2.0-rc1", "0.2.0", false},
		{"0.2.0", "dev", true},
		{"garbage", "0.1.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

type fakeGitHub struct {
	*httptest.Server
	tag    string
	binary []byte
	sums   string // SHA256SUMS content; computed when empty
	noRel  bool
}

func newFakeGitHub(t *testing.T, tag string, binary []byte) *fakeGitHub {
	f := &fakeGitHub{tag: tag, binary: binary}
	name := AssetName(runtime.GOOS, runtime.GOARCH)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			if f.noRel {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"tag_name": f.tag,
				"body":     "- 修复若干问题",
				"html_url": "https://example.test/release",
				"assets": []map[string]string{
					{"name": name, "browser_download_url": f.URL + "/dl/" + name},
					{"name": "SHA256SUMS", "browser_download_url": f.URL + "/dl/SHA256SUMS"},
				},
			})
		case "/dl/" + name:
			w.Write(f.binary)
		case "/dl/SHA256SUMS":
			sums := f.sums
			if sums == "" {
				h := sha256.Sum256(f.binary)
				sums = hex.EncodeToString(h[:]) + "  go-dock_other_arch\n" + hex.EncodeToString(h[:]) + "  " + name + "\n"
			}
			io.WriteString(w, sums)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func newTestUpdater(t *testing.T, current, api string) (*Updater, string) {
	exe := filepath.Join(t.TempDir(), "go-dock")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GO_DOCK_UPDATE_API", api)
	u := New(current, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	u.exe = func() (string, error) { return exe, nil }
	u.verify = func(ctx context.Context, path, version string) error { return nil }
	return u, exe
}

func TestCheckAndApply(t *testing.T) {
	gh := newFakeGitHub(t, "v0.2.0", []byte("new binary"))
	u, exe := newTestUpdater(t, "0.1.0", gh.URL)

	var verified string
	u.verify = func(ctx context.Context, path, version string) error {
		verified = version
		if b, _ := os.ReadFile(path); string(b) != "new binary" {
			return errors.New("verify saw wrong content")
		}
		return nil
	}
	if err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := u.Status()
	if !st.Available || st.Latest != "0.2.0" || st.Notes == "" || st.CheckedAt.IsZero() {
		t.Fatalf("status = %+v", st)
	}
	if err := u.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new binary" {
		t.Fatalf("exe content = %q", b)
	}
	if verified != "0.2.0" || u.Status().State != "restarting" {
		t.Fatalf("verified %q, status %+v", verified, u.Status())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".go-dock-update-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestApplyRejectsBadChecksum(t *testing.T) {
	gh := newFakeGitHub(t, "v0.2.0", []byte("tampered"))
	gh.sums = strings.Repeat("0", 64) + "  " + AssetName(runtime.GOOS, runtime.GOARCH) + "\n"
	u, exe := newTestUpdater(t, "0.1.0", gh.URL)
	u.Check(context.Background())
	err := u.Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary" {
		t.Fatal("exe replaced despite bad checksum")
	}
	if st := u.Status(); st.State != "error" || st.Error == "" {
		t.Fatalf("status = %+v", st)
	}
}

func TestApplyRejectsBrokenBinary(t *testing.T) {
	gh := newFakeGitHub(t, "v0.2.0", []byte("new binary"))
	u, exe := newTestUpdater(t, "0.1.0", gh.URL)
	u.verify = func(context.Context, string, string) error { return errors.New("exec format error") }
	u.Check(context.Background())
	if err := u.Apply(context.Background()); err == nil {
		t.Fatal("broken binary accepted")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary" {
		t.Fatal("exe replaced by a binary that does not run")
	}
}

func TestNothingToApply(t *testing.T) {
	gh := newFakeGitHub(t, "v0.1.0", []byte("same"))
	u, _ := newTestUpdater(t, "0.1.0", gh.URL)
	u.Check(context.Background())
	if u.Status().Available {
		t.Fatal("same version reported as available")
	}
	if err := u.Apply(context.Background()); err == nil {
		t.Fatal("apply without update succeeded")
	}

	gh.noRel = true
	if err := u.Check(context.Background()); err != nil {
		t.Fatalf("no releases yet should not be an error: %v", err)
	}
	if st := u.Status(); st.Latest != "" || st.Available {
		t.Fatalf("status = %+v", st)
	}
}

func TestDevBuildCannotApply(t *testing.T) {
	gh := newFakeGitHub(t, "v0.2.0", []byte("new"))
	u, _ := newTestUpdater(t, "dev", gh.URL)
	u.Check(context.Background())
	st := u.Status()
	if !st.Dev || !st.Available {
		t.Fatalf("status = %+v", st)
	}
	if err := u.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "开发版本") {
		t.Fatalf("err = %v", err)
	}
}

func TestMissingPlatformAsset(t *testing.T) {
	gh := newFakeGitHub(t, "v0.2.0", []byte("new"))
	u, _ := newTestUpdater(t, "0.1.0", gh.URL)
	u.goos, u.goarch = "plan9", "mips"
	u.Check(context.Background())
	if err := u.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "go-dock_plan9_mips") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunVersion(t *testing.T) {
	// A file that is not a program must be rejected quickly.
	path := filepath.Join(t.TempDir(), "go-dock")
	os.WriteFile(path, []byte("not a binary"), 0o755)
	if err := runVersion(context.Background(), path, "0.2.0"); err == nil {
		t.Fatal("non-executable accepted")
	}
}
