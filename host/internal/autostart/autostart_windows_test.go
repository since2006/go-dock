package autostart

import (
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/windows/registry"
)

func TestRunKey(t *testing.T) {
	parent := fmt.Sprintf(`Software\go-dock-test-%d`, os.Getpid())
	old := runKey
	runKey = parent + `\Run`
	t.Cleanup(func() {
		runKey = old
		registry.DeleteKey(registry.CURRENT_USER, parent+`\Run`)
		registry.DeleteKey(registry.CURRENT_USER, parent)
	})
	exe := `C:\Program Files\go-dock\go-dock.exe`

	if st := Get(exe); st.Enabled || st.Method != "registry" {
		t.Fatalf("before = %+v", st)
	}
	if err := Enable(exe); err != nil {
		t.Fatal(err)
	}
	if st := Get(exe); !st.Enabled || st.Stale {
		t.Fatalf("after enable = %+v", st)
	}
	k, _ := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	v, _, _ := k.GetStringValue(valueName)
	k.Close()
	if v != `"C:\Program Files\go-dock\go-dock.exe" run --background` {
		t.Fatalf("value = %q", v)
	}
	if st := Get(`C:\other\go-dock.exe`); !st.Stale {
		t.Fatal("moved exe not stale")
	}
	if err := Disable(); err != nil {
		t.Fatal(err)
	}
	if err := Disable(); err != nil {
		t.Fatal("second disable:", err)
	}
	if st := Get(exe); st.Enabled {
		t.Fatalf("after disable = %+v", st)
	}
}
