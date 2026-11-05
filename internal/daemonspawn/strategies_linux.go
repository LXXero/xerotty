package daemonspawn

import (
	"os"
	"os/exec"
	"os/user"
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
//
// Only when lingering is enabled for the user. Without it logind stops
// user@<uid>.service shortly after the user's last session ends
// (UserStopDelaySec, 10 s by default), and that takes app.slice — the
// daemon and every tab — with it. The old session placement survived
// a logout precisely because the daemon kept its session alive in
// "closing"; a scope must not trade that for polkit correctness. With
// linger off the fork strategy keeps the old lifetime; the fix is
// `loginctl enable-linger`, and the daemon log says so once.
var systemdScope = strategy{
	name: "systemd user scope",
	available: func() bool {
		return userManagerReachable() && lingerEnabled()
	},
	unavailableReason: func() string {
		if userManagerReachable() && !lingerEnabled() {
			return "systemd user manager found but lingering is off, so the daemon stays in this login session (enable with: loginctl enable-linger)"
		}
		return ""
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

// userManagerReachable reports whether `systemd-run --user` has a
// manager to talk to. The manager's private socket is the one
// reliable sign: over ssh without pam_systemd, or when nobody is
// logged in and linger is off, XDG_RUNTIME_DIR is unset or the
// manager isn't running.
func userManagerReachable() bool {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(rt, "systemd", "private"))
	return err == nil
}

// lingerDir is where logind records `loginctl enable-linger`: one
// file per user name. A variable so tests can point it elsewhere.
var lingerDir = "/var/lib/systemd/linger"

// lingerEnabled reports whether logind will keep this user's manager
// running after the last session ends.
func lingerEnabled() bool {
	u, err := user.Current()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(lingerDir, u.Username))
	return err == nil
}
