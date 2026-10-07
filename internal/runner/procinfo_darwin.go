//go:build darwin

package runner

import (
	"os/exec"
	"strconv"
	"strings"
)

// daemonChild returns the pid of the `serve --child` process whose
// parent is ppid, or 0 when there is none.
func daemonChild(ppid int) int {
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(ppid), "-f", "serve --child").Output()
	if err != nil {
		return 0
	}
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil && pid > 0 {
			return pid
		}
	}
	return 0
}

// exePath is the path pid was started from (ps prints the full path
// as comm on macOS).
func exePath(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// mapsFile: macOS has no /proc/<pid>/maps, so which binary a process
// runs cannot be checked; callers fall back to "the pid changed".
func mapsFile(int, fileID) (bool, string, error) {
	return false, "", errNoMaps
}
