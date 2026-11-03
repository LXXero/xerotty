package daemonspawn

import (
	"os"
	"os/exec"
	"path/filepath"
)

func strategies() []strategy {
	return []strategy{systemdScope, forkStrategy}
}

// systemdScope runs the daemon in a transient scope under the user's
// systemd manager (user@<uid>.service/app.slice). That is outside
// every login session: logind reports no session for it, polkit's
// implicit "active local user" rules apply (verified: the inhibit
// actions denied from a session-3 tab succeed here), and
// `loginctl terminate-session` can't reach it.
//
// `--scope` (not a service) because systemd-run then execs the daemon
// in place: the process we start is the daemon, so exit detection and
// stderr wiring work exactly as for a plain fork, and a scope has no
// main PID whose death would make systemd kill the tab shells.
// `--collect` unloads the scope once empty even after a failure.
var systemdScope = strategy{
	name: "systemd user scope",
	available: func() bool {
		if _, err := exec.LookPath("systemd-run"); err != nil {
			return false
		}
		// The user manager's private socket is the one reliable sign
		// that `systemd-run --user` has someone to talk to. Over ssh
		// without pam_systemd, or on systems where lingering is off
		// and nobody is logged in, XDG_RUNTIME_DIR is unset or the
		// manager isn't running — fall through to fork.
		rt := os.Getenv("XDG_RUNTIME_DIR")
		if rt == "" {
			return false
		}
		_, err := os.Stat(filepath.Join(rt, "systemd", "private"))
		return err == nil
	},
	start: func(o Options, env []string) (handle, error) {
		args := []string{
			"--user", "--scope", "--quiet", "--collect",
			"--unit=" + UnitName(o.SocketPath),
			"--description=xerotty daemon (" + o.SocketPath + ")",
			"--", o.Self, "serve", "--socket", o.SocketPath,
		}
		cmd := exec.Command("systemd-run", args...)
		cmd.Env = env
		cmd.SysProcAttr = detachSysProcAttr()
		cmd.Stdin = nil
		cmd.Stdout = nil
		// systemd-run's own failure text ("Failed to start transient
		// scope unit: ...") lands in the daemon log too.
		cmd.Stderr = o.Stderr
		if err := cmd.Start(); err != nil {
			return handle{}, err
		}
		return handle{
			pid:    cmd.Process.Pid,
			exited: waitExit(cmd),
			abort:  func() { _ = cmd.Process.Kill() },
		}, nil
	},
}
