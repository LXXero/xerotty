// Package terminal manages PTY allocation, shell spawning, and the SafeEmulator lifecycle.
package terminal

import (
	"os"
	"os/exec"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/creack/pty"
)

// spawnPTY starts the configured shell inside a new PTY. cwd is the
// working directory for the shell; empty string lets exec inherit the
// xerotty process's CWD (the usual "open new tab from launcher"
// behavior). Set to the parent tab's CWD when cfg.Tabs.InheritCWD is
// on so "New Tab" picks up wherever the user is in the previous tab.
func spawnPTY(cfg *config.Config, cols, rows uint16, cwd string) (*os.File, *exec.Cmd, error) {
	shell := cfg.DetectShell()
	cmd := exec.Command(shell)
	if cwd != "" {
		cmd.Dir = cwd
	}

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
