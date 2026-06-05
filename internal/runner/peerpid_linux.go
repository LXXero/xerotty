//go:build linux

package runner

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerPID returns the pid of the process on the other end of a unix
// socket connection — how `serve --upgrade` finds the running
// daemon without a pidfile.
func peerPID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var gerr error
	if err := raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			gerr = err
			return
		}
		pid = int(cred.Pid)
	}); err != nil {
		return 0, err
	}
	if gerr != nil {
		return 0, gerr
	}
	if pid <= 0 {
		return 0, fmt.Errorf("peer pid unavailable")
	}
	return pid, nil
}
