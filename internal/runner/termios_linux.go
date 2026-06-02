//go:build linux

package runner

import "golang.org/x/sys/unix"

// Linux's tty ioctls use the TCGETS / TCSETS variants on the modern
// termios struct. golang.org/x/sys/unix doesn't define TIOCGETA on
// linux (which is the BSD/Darwin convention) so makeRaw / restoreTerm
// pick the right pair per-OS through this shim.
const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)
