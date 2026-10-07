//go:build darwin

package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// daemonChild returns the pid of the `serve --child` process whose
// parent is ppid, or 0 when there is none. The children come from
// pgrep -P and each argv from ps, matched by isDaemonChildArgv: a
// child that exec'd in place has `--child` after `--resume`, not next
// to `serve`, so a `pgrep -f "serve --child"` substring misses it and
// the CLI would wait out the handoff and SIGKILL a child that
// upgraded fine.
func daemonChild(ppid int) int {
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(ppid)).Output()
	if err != nil {
		return 0
	}
	for _, f := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(f)
		if err != nil || pid <= 0 {
			continue
		}
		if info, err := procInfoOf(pid); err == nil && isDaemonChildArgv(info.argv) {
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

// exePath is the path of the executable pid runs. `ps -o comm=` prints
// argv[0] as the process was started: the full path when it was
// launched by path, but a bare "xerotty" when a shell or the GUI found
// it on PATH, and that name must not be taken for a file (relative to
// the caller's cwd it stats whatever happens to be there and the
// validation gate's exec then fails the PATH lookup). A relative comm
// is resolved from lsof's txt vnodes, whose first entry is the
// executable's path; with no lsof the caller falls back to its own
// executable.
func exePath(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	comm := strings.TrimSpace(string(out))
	if filepath.IsAbs(comm) {
		return comm, nil
	}
	lsof := lsofPath()
	if lsof == "" {
		return "", fmt.Errorf("pid %d: comm %q is not a path and lsof is unavailable", pid, comm)
	}
	out, err = exec.Command(lsof, "-w", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-F", "Din").Output()
	if err != nil {
		return "", fmt.Errorf("lsof -p %d: %w", pid, err)
	}
	_, exe := lsofTxt(out, fileID{})
	if !filepath.IsAbs(exe) {
		return "", fmt.Errorf("pid %d: lsof lists no executable", pid)
	}
	return exe, nil
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
