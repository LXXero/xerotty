package terminal

import (
	"os/exec"
	"strconv"
	"strings"
)

// processName returns the executable name (argv[0] without path) for
// pid. macOS implementation uses `ps -o comm=` which prints just the
// command name. Returns "" if pid is gone or ps fails.
func processName(pid int) string {
	if pid <= 0 {
		return ""
	}
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(out))
	// ps -o comm= can return the full path on macOS (e.g. /usr/bin/vim).
	// Strip to basename.
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return name
}
