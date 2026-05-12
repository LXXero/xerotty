//go:build !darwin

package app

import (
	"os"
	"os/exec"
)

// newWindowCommand returns the exec.Cmd to spawn a fresh xerotty
// window. On Linux there's no LaunchServices equivalent — plain
// fork+exec is what every WM expects.
func newWindowCommand(exe string, envExtras []string) *exec.Cmd {
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), envExtras...)
	return cmd
}
