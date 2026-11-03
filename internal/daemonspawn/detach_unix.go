//go:build !windows

package daemonspawn

import "syscall"

// detachSysProcAttr starts the daemon in a new session: its own
// process group and no controlling tty, so the spawner's tty hanging
// up (ssh session ending, terminal window closing) can't reach it
// with SIGHUP. Setpgid alone — what the old spawn used — leaves the
// controlling tty attached.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
