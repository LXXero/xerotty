// Package terminal manages PTY allocation, shell spawning, and the SafeEmulator lifecycle.
package terminal

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/creack/pty"
)

// LaunchCmd is an optional program override for a tab's PTY (the
// `xterm -e` / `-x` feature). Argv is the program plus its arguments;
// when Shell is set, Argv is joined and run through the configured
// shell's `-c` so pipes/globs/`&&` work. A nil *LaunchCmd (or empty
// Argv) runs the default login/interactive shell — the normal tab.
type LaunchCmd struct {
	Argv  []string
	Shell bool
}

// spawnPTY starts the configured shell — or, when launch is non-nil,
// the override command — inside a new PTY. cwd is the working
// directory; empty string lets exec inherit the xerotty process's CWD
// (the usual "open new tab from launcher" behavior). Set to the parent
// tab's CWD when cfg.Tabs.InheritCWD is on so "New Tab" picks up
// wherever the user is in the previous tab.
func spawnPTY(cfg *config.Config, cols, rows uint16, cwd string, launch *LaunchCmd) (*os.File, *exec.Cmd, error) {
	shell := cfg.DetectShell()
	var cmd *exec.Cmd
	switch {
	case launch != nil && len(launch.Argv) > 0 && launch.Shell:
		// `-x` / `sh -c`: run the command string through the shell so
		// pipes, &&, and globs work. Argv is the single launch string.
		cmd = exec.Command(shell, "-c", strings.Join(launch.Argv, " "))
	case launch != nil && len(launch.Argv) > 0:
		// `-e`: exec the program + args directly, no shell, no quoting
		// surprises.
		cmd = exec.Command(launch.Argv[0], launch.Argv[1:]...)
	default:
		cmd = exec.Command(shell)
		if runtime.GOOS == "darwin" {
			// Spawn a LOGIN shell on macOS (leading "-" in argv[0]), like
			// Terminal.app and iTerm2 do. A Finder-launched app inherits
			// launchd's bare env (no /opt/homebrew/bin on PATH), and only a
			// login shell reads ~/.zprofile — where Homebrew/starship/mise
			// PATH setup conventionally lives on macOS. Without this,
			// Finder-launched tabs get a shell that can't find brew-installed
			// tools. Linux is untouched: terminals there spawn non-login
			// interactive shells and the session env is already complete.
			// Skipped for an explicit command — that's not a login shell.
			cmd.Args[0] = "-" + filepath.Base(shell)
		}
	}
	if cwd != "" {
		cmd.Dir = cwd
	}

	// Build environment
	cmd.Env = append(os.Environ(),
		"TERM="+cfg.Term,
		"COLORTERM=truecolor",
	)
	if os.Getenv("LC_ALL") == "" && os.Getenv("LC_CTYPE") == "" && os.Getenv("LANG") == "" {
		// A locale-less environment (launchd on macOS; on Linux a
		// daemon spawned from an early-boot / display-manager context
		// that never sourced locale.conf) lands the shell in the C
		// locale, where zsh — with MULTIBYTE off — counts UTF-8
		// prompt glyphs (starship's Powerline / Nerd Font PUA chars)
		// as bytes. Every ZLE redraw then repositions the cursor too
		// far right and repaints the buffer offset — "cd .cacd .ca"
		// style duplication on each tab-completion. Terminal.app,
		// iTerm2, and kitty all set LANG themselves for exactly this
		// reason. Only defaulted when no locale is present; cfg.Env
		// (below) can override. Was darwin-only until the identical
		// trap bit a Linux daemon that outlived its login session.
		cmd.Env = append(cmd.Env, "LANG=en_US.UTF-8")
	}
	// Same disease, display flavor (Linux): a daemon started from a
	// tty/SSH context has no WAYLAND_DISPLAY/DISPLAY, and it outlives
	// (and via exec-in-place upgrades, preserves) that blindness. Its
	// tabs then fail every "is a GUI available" probe — mutt's mailcap
	// test picking a text browser over firefox was the live case. An
	// interactive zsh can self-heal in zshrc, but a direct-command tab
	// (`-e mutt`) never runs rc files, so patch it here: when the var
	// is absent but the session's socket actually exists, point at it.
	// Empty vars stay empty when no compositor/Xwayland is up (headless
	// server daemon) — probes then fail correctly.
	cmd.Env = append(cmd.Env, displayEnvDefaults()...)
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
