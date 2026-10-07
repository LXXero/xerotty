//go:build darwin

package runner

import (
	"fmt"
	"os"
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

// procInfoOf asks ps for pid's parent and command line. argv is the
// command line split on blanks, so an argument holding a space comes
// back as several; isDaemonChildArgv only looks for whole words.
func procInfoOf(pid int) (procInfo, error) {
	out, err := exec.Command("ps", "-ww", "-o", "ppid=", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return procInfo{}, fmt.Errorf("ps -p %d: %w", pid, err)
	}
	f := strings.Fields(string(out))
	if len(f) < 2 {
		return procInfo{}, fmt.Errorf("ps -p %d: no such process", pid)
	}
	ppid, err := strconv.Atoi(f[0])
	if err != nil {
		return procInfo{}, fmt.Errorf("ps -p %d: ppid %q", pid, f[0])
	}
	return procInfo{ppid: ppid, argv: f[1:]}, nil
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

// mapsFile reports whether pid runs id. macOS has no
// /proc/<pid>/maps; lsof lists the vnodes a process has mapped as
// txt (its executable first) from the kernel's region info, and
// shelling out to it keeps this file free of cgo. Without lsof which
// binary a process runs cannot be checked: errNoMaps, and callers
// fall back to the dropped connection and the handshake.
func mapsFile(pid int, id fileID) (bool, string, error) {
	lsof := lsofPath()
	if lsof == "" {
		return false, "", errNoMaps
	}
	out, err := exec.Command(lsof, "-w", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-F", "Din").Output()
	if err != nil {
		return false, "", fmt.Errorf("lsof -p %d: %w", pid, err)
	}
	ok, mapped := lsofTxt(out, id)
	return ok, mapped, nil
}

// lsofPath finds lsof even when PATH lacks /usr/sbin, where macOS
// ships it.
func lsofPath() string {
	if p, err := exec.LookPath("lsof"); err == nil {
		return p
	}
	if _, err := os.Stat("/usr/sbin/lsof"); err == nil {
		return "/usr/sbin/lsof"
	}
	return ""
}
