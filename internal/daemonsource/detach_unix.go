//go:build !windows

package daemonsource

import "syscall"

// detachSysProcAttr returns a SysProcAttr that puts the daemon in
// its own process group + session so the GUI quitting doesn't drag
// it down via SIGHUP / SIGTERM-to-pgroup.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setpgid: true,
		// Setsid would also work but Setpgid is sufficient and
		// portable across Linux/macOS without depending on a
		// controlling tty's state.
	}
}
