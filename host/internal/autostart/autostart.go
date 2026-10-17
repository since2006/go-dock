// Package autostart starts go-dock when the user logs in, using the
// platform's per-user mechanism: a LaunchAgent on macOS, a systemd user
// unit or XDG autostart entry on Linux, and the HKCU Run key on Windows.
//
// Enabling never starts a second instance right away; it takes effect at
// the next login.
package autostart

// Args are the arguments the login item passes to go-dock.
var Args = []string{"run", "--background"}

// Status describes the login item as shown on the web UI.
type Status struct {
	Supported bool   `json:"supported"`
	Enabled   bool   `json:"enabled"`
	Method    string `json:"method,omitempty"` // launchd, systemd, xdg, registry
	Path      string `json:"path,omitempty"`   // file or registry location
	// Stale means the login item points at another executable.
	Stale bool   `json:"stale"`
	Error string `json:"error,omitempty"`
}

// Get reports the login item state for exe.
func Get(exe string) Status {
	st, err := get(exe)
	if err != nil {
		st.Error = err.Error()
	}
	return st
}

// Enable registers exe to start at login, replacing an older entry.
func Enable(exe string) error { return enable(exe) }

// Disable removes the login item; it does not stop the running instance.
func Disable() error { return disable() }
