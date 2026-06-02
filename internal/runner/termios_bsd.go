//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package runner

import "golang.org/x/sys/unix"

// BSD-family (and Darwin, which inherits the BSD tty layer) uses the
// TIOCGETA / TIOCSETA pair — TCGETS/TCSETS aren't defined in unix on
// these platforms. See termios_linux.go for the Linux variant.
const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)
