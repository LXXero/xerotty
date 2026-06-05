//go:build darwin

package runner

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerPID returns the pid of the process on the other end of a unix
// socket connection. macOS spells SO_PEERCRED as LOCAL_PEERPID on
// the SOL_LOCAL level.
func peerPID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var gerr error
	if err := raw.Control(func(fd uintptr) {
		p, err := unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		if err != nil {
			gerr = err
			return
		}
		pid = p
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
