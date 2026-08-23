package terminal

import (
	"fmt"
	"os"
	"path/filepath"
)

// displayEnvDefaults returns WAYLAND_DISPLAY/DISPLAY entries for a
// spawned PTY when the daemon's own environment lacks them but the
// session's sockets exist on disk — the daemon-outlived-its-launch-
// context case (see the call site in spawnPTY). Checked per spawn,
// not cached: the compositor can come and go under a long-lived
// daemon, and a fresh tab should see the current truth.
func displayEnvDefaults() []string {
	var out []string
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
			if fi, err := os.Stat(filepath.Join(dir, "wayland-0")); err == nil && fi.Mode()&os.ModeSocket != 0 {
				out = append(out, "WAYLAND_DISPLAY=wayland-0")
			}
		}
	}
	if os.Getenv("DISPLAY") == "" {
		// Lowest live X socket wins — :0 in any single-seat setup
		// (Xwayland's default). Sockets are named /tmp/.X11-unix/X<n>.
		for n := 0; n < 3; n++ {
			if fi, err := os.Stat(fmt.Sprintf("/tmp/.X11-unix/X%d", n)); err == nil && fi.Mode()&os.ModeSocket != 0 {
				out = append(out, fmt.Sprintf("DISPLAY=:%d", n))
				break
			}
		}
	}
	return out
}
