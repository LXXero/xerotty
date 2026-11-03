// Package daemonspawn starts `xerotty serve` on behalf of a client
// that found no daemon listening, and puts it somewhere it can outlive
// that client *and* its login session.
//
// Why placement matters: the daemon used to be a plain child of the
// first client, detached only with setpgid. A process group is not a
// session. logind tracks sessions by cgroup, so the daemon — and every
// tab shell it ever spawns — stayed in the spawner's session scope for
// good. On every machine checked (2026-11-03) that first client was an
// SSH session, three of them long dead and held in "closing" only by
// the daemon. Consequences, each verified:
//
//   - polkit treats a remote session as a remote login:
//     `systemd-inhibit`, udisks mounts, power actions, rfkill and
//     NetworkManager changes from any tab fail with "Access denied".
//   - tabs inherit SSH_CLIENT / SSH_CONNECTION / XDG_SESSION_TYPE=tty,
//     so anything that asks "am I over SSH?" misfires.
//   - `loginctl terminate-session <n>` (or KillUserProcesses=yes)
//     takes every terminal with it.
//   - in-place upgrades (`serve --upgrade`) re-exec and so preserve
//     all of the above forever.
//
// Strategies, tried in order until one gets the socket up:
//
//   - Linux with a reachable systemd user manager: a transient scope
//     (`systemd-run --user --scope`) under user@.service/app.slice.
//     Outside any login session; polkit sees an active local user.
//   - macOS with a GUI login: a launchd job bootstrapped into the
//     user's gui domain, so the daemon has the Aqua session
//     (pasteboard, WindowServer) even when spawned over SSH.
//   - everywhere else (non-systemd Linux, BSDs, macOS over SSH with no
//     GUI login, Windows): fork with setsid — a real new session, no
//     controlling tty — which is the most any such system offers.
//
// All strategies drop the spawner's session-identity variables (SSH_*
// identity, XDG_SESSION_*, XDG_SEAT, XDG_VTNR) from the daemon's
// environment: once the daemon has outlived the session they describe,
// they are lies that tab programs act on.
package daemonspawn

import (
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Options describes the daemon to start.
type Options struct {
	// Self is the xerotty binary to exec (one binary, several roles).
	Self string
	// SocketPath is passed as `serve --socket`.
	SocketPath string
	// Stderr receives the daemon's log output. nil means /dev/null.
	// Spawners must never hand the daemon their own stderr: over ssh
	// that is the session's pipe, and Go re-raises EPIPE on fd 2 as
	// SIGPIPE, so the daemon would die on its first log line after the
	// session ends (see sockpath.DaemonLogFile).
	Stderr *os.File
	// Dial reports whether the daemon is accepting connections.
	Dial func() bool
	// Timeout bounds the wait for Dial to succeed; 0 means 2s.
	// Daemons normally bind in well under a second; the slack is for
	// loaded VMs and small boards.
	Timeout time.Duration
	// Env is the spawner's environment; nil means os.Environ().
	Env []string
}

// handle is what a started strategy hands back for supervision.
type handle struct {
	// pid of the daemon when the strategy started it directly (0 when
	// another manager owns it). Tests use it to check placement.
	pid int
	// exited is closed once the spawned process is known to have
	// exited. nil when the strategy cannot know (launchd owns the
	// process).
	exited <-chan struct{}
	// abort is best-effort cleanup when the socket never came up.
	abort func()
}

// strategy is one way of starting the daemon.
type strategy struct {
	name      string
	available func() bool
	start     func(o Options, env []string) (handle, error)
}

// EnsureListening starts the daemon and waits for o.Dial to report it
// up. It returns the name of the strategy that worked, for logs.
func EnsureListening(o Options) (string, error) {
	return ensureListening(o, strategies())
}

func ensureListening(o Options, candidates []strategy) (string, error) {
	if o.Dial == nil {
		return "", fmt.Errorf("daemon spawn: no Dial probe")
	}
	if o.Timeout <= 0 {
		o.Timeout = 2 * time.Second
	}
	env := DaemonEnv(o.Env)
	if o.Env == nil {
		env = DaemonEnv(os.Environ())
	}

	var tried []string
	for _, s := range candidates {
		if s.available != nil && !s.available() {
			continue
		}
		h, err := s.start(o, env)
		if err != nil {
			o.logf("spawn via %s failed: %v", s.name, err)
			tried = append(tried, s.name+": "+err.Error())
			continue
		}
		deadline := time.Now().Add(o.Timeout)
		exitedEarly := false
		for time.Now().Before(deadline) && !exitedEarly {
			if o.Dial() {
				return s.name, nil
			}
			select {
			case <-h.exited:
				// One last look: a bind racing with the exit
				// notification would otherwise be misread.
				if o.Dial() {
					return s.name, nil
				}
				exitedEarly = true
			case <-time.After(40 * time.Millisecond):
			}
		}
		if exitedEarly {
			o.logf("spawn via %s: daemon exited before binding %s; trying next", s.name, o.SocketPath)
			tried = append(tried, s.name+": exited before binding")
			continue
		}
		// Still running but not listening. Kill it rather than leave a
		// second daemon racing a later spawn for the same socket.
		if h.abort != nil {
			h.abort()
		}
		return "", fmt.Errorf("daemon spawn via %s: socket %s never came up within %s", s.name, o.SocketPath, o.Timeout)
	}
	return "", fmt.Errorf("daemon spawn: every strategy failed (%s)", strings.Join(tried, "; "))
}

// logf writes a diagnostic to the daemon log, where the daemon's own
// first lines will follow it.
func (o Options) logf(format string, args ...any) {
	if o.Stderr == nil {
		return
	}
	fmt.Fprintf(o.Stderr, "xerotty spawn: "+format+"\n", args...)
}

// sessionIdentity lists environment variables that describe the
// spawner's login session. The daemon outlives that session, so in
// its (and every tab's) environment they would be stale at best and
// wrong at worst: SSH_CLIENT in a tab makes agents and prompts behave
// as if the user were remote. SSH_AUTH_SOCK is kept on purpose — it is
// functional, not descriptive, and users who forward an agent want it.
var sessionIdentity = []string{
	"SSH_CLIENT",
	"SSH_CONNECTION",
	"SSH_TTY",
	"XDG_SESSION_ID",
	"XDG_SESSION_TYPE",
	"XDG_SESSION_CLASS",
	"XDG_SESSION_DESKTOP",
	"XDG_SEAT",
	"XDG_VTNR",
}

// DaemonEnv returns environ without the session-identity variables.
func DaemonEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
next:
	for _, kv := range environ {
		for _, k := range sessionIdentity {
			if strings.HasPrefix(kv, k+"=") {
				continue next
			}
		}
		out = append(out, kv)
	}
	return out
}

// UnitName is the systemd scope / launchd label stem for a daemon
// serving socketPath: a stable prefix for listing, a hash of the path
// so distinct sockets never collide, and a per-spawn suffix so a dead
// daemon whose scope is still held open by surviving tab shells can't
// block its replacement.
func UnitName(socketPath string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(socketPath))
	return "xerotty-daemon-" + strconv.FormatUint(h.Sum64(), 36)[:8] +
		"-" + strconv.FormatInt(time.Now().UnixMilli(), 36)
}

// waitExit reaps cmd in the background and reports its exit.
func waitExit(cmd *exec.Cmd) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	return done
}

// forkStrategy is the universal fallback: exec self directly in a new
// session (setsid where the OS has it).
var forkStrategy = strategy{
	name:      "fork",
	available: func() bool { return true },
	start: func(o Options, env []string) (handle, error) {
		cmd := exec.Command(o.Self, "serve", "--socket", o.SocketPath)
		cmd.Env = env
		cmd.SysProcAttr = detachSysProcAttr()
		cmd.Stdin = nil
		cmd.Stdout = nil
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
