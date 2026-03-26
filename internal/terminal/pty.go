// Package terminal manages PTY allocation, shell spawning, and the SafeEmulator lifecycle.
package terminal

import (
	"os"
	"os/exec"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/creack/pty"
)

// spawnPTY starts the configured shell inside a new PTY.
// Returns the PTY file descriptor and the child process cmd.
func spawnPTY(cfg *config.Config, cols, rows uint16) (*os.File, *exec.Cmd, error) {
	shell := cfg.DetectShell()
	cmd := exec.Command(shell)

	// Build environment
	cmd.Env = append(os.Environ(),
		"TERM="+cfg.Term,
		"COLORTERM=truecolor",
	)
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	// Start with initial size
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Rows: rows,
		Cols: cols,
	})
	if err != nil {
		return nil, nil, err
	}

	return ptmx, cmd, nil
}
