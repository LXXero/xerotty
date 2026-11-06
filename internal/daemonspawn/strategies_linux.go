package daemonspawn

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
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
// Lifetime: a scope lives exactly as long as the user manager, and
// logind stops user@<uid>.service shortly after the user's last
// session ends (UserStopDelaySec, 10 s by default) unless lingering is
// on. The old session placement survived a logout because the daemon
// held its session open in "closing"; a scope must not quietly trade
// that away. So the scope is used only when something will keep the
// manager alive: lingering, or a console login (a local, non-remote
// session of this user, the thing that stays up for weeks on a desktop
// while SSH sessions come and go). With neither — a headless box
// reached only over SSH — the fork strategy keeps the old lifetime and
// the daemon log says why. Lingering remains the unconditional answer.
var systemdScope = strategy{
	name: "systemd user scope",
	available: func() bool {
		return userManagerReachable() && (lingerEnabled() || consoleSessionAlive())
	},
	unavailableReason: func() string {
		if userManagerReachable() && !lingerEnabled() && !consoleSessionAlive() {
			return "systemd user manager found, but neither lingering nor a console login would keep it alive after this session ends; daemon stays in this login session (loginctl enable-linger makes the scope unconditional)"
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

// runLoginctl runs loginctl and returns its stdout. A variable so
// tests can feed canned sessions.
var runLoginctl = func(args ...string) (string, error) {
	out, err := exec.Command("loginctl", args...).Output()
	return string(out), err
}

// sessionInfo is what the scope gate needs to know about one logind
// session, from `loginctl show-session`.
type sessionInfo struct {
	class  string // "user", "manager", "greeter", "background", …
	remote bool   // Remote=yes: ssh and friends
	state  string // "active", "online", "closing"
}

// consoleSessionAlive reports whether this user has a local login
// session that is not on its way out. Such a session keeps the user
// manager — and so a scope — alive without lingering.
func consoleSessionAlive() bool {
	out, err := runLoginctl("show-user", strconv.Itoa(os.Getuid()), "-p", "Sessions")
	if err != nil {
		return false
	}
	var infos []sessionInfo
	for _, id := range strings.Fields(strings.TrimPrefix(strings.TrimSpace(out), "Sessions=")) {
		raw, err := runLoginctl("show-session", id, "-p", "Class", "-p", "Remote", "-p", "State")
		if err != nil {
			continue
		}
		infos = append(infos, parseSessionInfo(raw))
	}
	return anyConsoleSession(infos)
}

// parseSessionInfo reads the Key=Value lines of `loginctl show-session
// -p Class -p Remote -p State`. Order is not fixed, so no positional
// --value parsing.
func parseSessionInfo(raw string) sessionInfo {
	var si sessionInfo
	for _, line := range strings.Split(raw, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "Class":
			si.class = v
		case "Remote":
			si.remote = v == "yes"
		case "State":
			si.state = v
		}
	}
	return si
}

// anyConsoleSession is the gate's rule: a real login (class user),
// local, and still active or online. "closing" is a session already
// torn down that lingering processes hold open — it will not outlive
// them — and class manager is user@ itself, which is what we are
// trying to keep alive, not evidence that something else will.
func anyConsoleSession(infos []sessionInfo) bool {
	for _, si := range infos {
		if si.class == "user" && !si.remote && (si.state == "active" || si.state == "online") {
			return true
		}
	}
	return false
}
