//go:build linux

package runner

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// daemonChild returns the pid of the `serve --child` process whose
// parent is ppid, or 0 when there is none.
func daemonChild(ppid int) int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		// cmdline only for ppid's children: this runs in poll loops.
		if p, err := procPPID(pid); err != nil || p != ppid {
			continue
		}
		if argv, err := procArgv(pid); err == nil && isDaemonChildArgv(argv) {
			return pid
		}
	}
	return 0
}

func procInfoOf(pid int) (procInfo, error) {
	ppid, err := procPPID(pid)
	if err != nil {
		return procInfo{}, err
	}
	argv, err := procArgv(pid)
	if err != nil {
		return procInfo{}, err
	}
	return procInfo{ppid: ppid, argv: argv}, nil
}

func procPPID(pid int) (int, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// comm may hold spaces and parens: the fields that follow it
	// start after the LAST ')'. Then: state, ppid, ...
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, fmt.Errorf("/proc/%d/stat: no comm", pid)
	}
	f := strings.Fields(string(stat[i+1:]))
	if len(f) < 2 {
		return 0, fmt.Errorf("/proc/%d/stat: no ppid", pid)
	}
	return strconv.Atoi(f[1])
}

func procArgv(pid int) ([]string, error) {
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00"), nil
}

// exePath is the path pid was started from. After an install replaced
// the file the kernel appends " (deleted)"; the path itself now names
// the new binary, which is what an upgrade will run.
func exePath(pid int) (string, error) {
	p, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(p, " (deleted)"), nil
}

// mapsFile reports whether pid has id mapped, from /proc/<pid>/maps —
// not /proc/<pid>/exe, whose path reads the same before and after an
// install replaced the file. mapped is the first file-backed mapping
// (the executable), for messages. A mapping matches on inode plus
// either device or path: btrfs subvolumes and overlayfs report a
// different device in maps than stat(2) does.
func mapsFile(pid int, id fileID) (ok bool, mapped string, err error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return false, "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// address perms offset dev inode path
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 {
			continue
		}
		ino, err := strconv.ParseUint(fields[4], 10, 64)
		if err != nil || ino == 0 {
			continue
		}
		path := strings.Join(fields[5:], " ")
		if mapped == "" {
			mapped = path
		}
		if ino != id.ino {
			continue
		}
		var maj, min uint64
		if _, err := fmt.Sscanf(fields[3], "%x:%x", &maj, &min); err == nil && unix.Mkdev(uint32(maj), uint32(min)) == id.dev {
			return true, path, nil
		}
		if strings.TrimSuffix(path, " (deleted)") == id.path {
			return true, path, nil
		}
	}
	return false, mapped, sc.Err()
}
