// Package terminal manages PTY allocation, shell spawning, and the SafeEmulator lifecycle.
package terminal

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

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
	if runtime.GOOS == "darwin" {
		// Spawn a LOGIN shell on macOS (leading "-" in argv[0]), like
		// Terminal.app and iTerm2 do. A Finder-launched app inherits
		// launchd's bare env (no /opt/homebrew/bin on PATH), and only a
		// login shell reads ~/.zprofile — where Homebrew/starship/mise
		// PATH setup conventionally lives on macOS. Without this,
		// Finder-launched tabs get a shell that can't find brew-installed
		// tools. Linux is untouched: terminals there spawn non-login
		// interactive shells and the session env is already complete.
		cmd.Args[0] = "-" + filepath.Base(shell)
	}
	if cwd != "" {
		cmd.Dir = cwd
	}

	// Build environment
	cmd.Env = append(os.Environ(),
		"TERM="+cfg.Term,
		"COLORTERM=truecolor",
	)
	if runtime.GOOS == "darwin" &&
		os.Getenv("LC_ALL") == "" && os.Getenv("LC_CTYPE") == "" && os.Getenv("LANG") == "" {
		// launchd's environment (what Finder-launched GUIs and the
		// daemons they auto-spawn inherit) carries NO locale vars, so
		// the shell lands in the C locale and zsh — with MULTIBYTE
		// off — mangles UTF-8 prompt glyphs (starship's Powerline /
		// Nerd Font PUA chars). Terminal.app, iTerm2, and kitty all
		// set LANG themselves for exactly this reason. Only defaulted
		// when no locale is present; cfg.Env (below) can override.
		cmd.Env = append(cmd.Env, "LANG=en_US.UTF-8")
	}
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
