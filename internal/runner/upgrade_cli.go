// `xerotty serve --upgrade`: find what owns the daemon socket, make
// it run the binary now installed, and only then say it worked. Three
// shapes of daemon can own the socket:
//
//   - a supervisor that handles SIGUSR2 (it writes a pidfile saying
//     so): it asks its child to hand off, starts the new binary, and
//     re-execs itself;
//   - a supervisor that predates that (no pidfile; it ignores
//     SIGUSR2): its child is SIGKILLed so the crash-resume path
//     restarts it from the binary on disk with every session;
//   - an unsupervised daemon: SIGUSR2 execs it in place, and the new
//     image adopts it into a supervisor.
//
// "Worked" is checked against the kernel, not inferred from the pid
// being alive: the processes must map the target binary's inode.

package runner

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/supervise"
)

const upgradeMsg = "xerotty serve --upgrade: "

// defaultUpgradeWait bounds how long --upgrade waits for the new code
// to be serving; XEROTTY_UPGRADE_TIMEOUT (a Go duration) overrides it.
const defaultUpgradeWait = 30 * time.Second

var errNoMaps = errors.New("this platform cannot show which binary a process runs")

// fileID identifies a binary on disk the way /proc/<pid>/maps does.
type fileID struct {
	dev, ino uint64
	path     string
}

func statFile(path string) (fileID, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileID{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}, fmt.Errorf("stat %s: no inode", path)
	}
	return fileID{dev: uint64(st.Dev), ino: uint64(st.Ino), path: path}, nil
}

// isDaemonChildArgv matches the argv a supervisor gives its daemon
// child: `<bin> serve --child ...`.
func isDaemonChildArgv(args []string) bool {
	serve := false
	for _, a := range args {
		switch a {
		case "serve":
			serve = true
		case "--child":
			return serve
		}
	}
	return false
}

func upgradeCLI(socketPath string) int {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, upgradeMsg+"no daemon on %s: %v\n", socketPath, err)
		return 1
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		conn.Close()
		fmt.Fprintln(os.Stderr, upgradeMsg+"not a unix socket connection")
		return 1
	}
	// SO_PEERCRED names the process that called listen(): the
	// supervisor when there is one, not the child serving the conn.
	pid, err := peerPID(uc)
	conn.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, upgradeMsg+"peer pid: %v\n", err)
		return 1
	}

	target := os.Getenv("XEROTTY_UPGRADE_BINARY")
	if target == "" {
		if target, err = exePath(pid); err != nil {
			target = upgradeTargetBinary()
		}
	}
	id, err := statFile(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, upgradeMsg+"target binary: %v\n", err)
		return 1
	}
	u := &upgrader{sock: socketPath, pid: pid, target: target, id: id, wait: defaultUpgradeWait}
	if d, err := time.ParseDuration(os.Getenv("XEROTTY_UPGRADE_TIMEOUT")); err == nil && d > 0 {
		u.wait = d
	}

	// A supervisor between children has none for a moment.
	child := 0
	for deadline := time.Now().Add(time.Second); child == 0 && time.Now().Before(deadline); {
		if child = daemonChild(pid); child == 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	pf, pfErr := supervise.ReadPIDFile(socketPath)
	switch {
	case pfErr == nil && pf.PID == pid && pf.Upgrade:
		return u.supervised(child)
	case child > 0:
		return u.oldSupervisor(child)
	default:
		return u.unsupervised()
	}
}

type upgrader struct {
	sock   string
	pid    int // owner of the socket
	target string
	id     fileID
	wait   time.Duration
}

// runs reports whether pid maps the target binary. Where that cannot
// be read, it is true: the caller's other checks (a new pid) are all
// there is.
func (u *upgrader) runs(pid int) bool {
	ok, _, err := mapsFile(pid, u.id)
	return ok || errors.Is(err, errNoMaps)
}

// describe says what pid runs, for failure messages.
func (u *upgrader) describe(pid int) string {
	ok, mapped, err := mapsFile(pid, u.id)
	switch {
	case err != nil:
		return "an unknown binary (" + err.Error() + ")"
	case ok:
		return u.target
	case mapped == "":
		return "an old binary"
	}
	return "the old binary " + mapped
}

func (u *upgrader) platformNote() string {
	if _, _, err := mapsFile(os.Getpid(), u.id); errors.Is(err, errNoMaps) {
		return " (" + err.Error() + ": checked that the daemon restarted, not which binary it runs)"
	}
	return ""
}

// until polls cond until it holds or u.wait passes. A non-nil error
// from cond ends the wait at once.
func (u *upgrader) until(cond func() (bool, error)) error {
	deadline := time.Now().Add(u.wait)
	for time.Now().Before(deadline) {
		ok, err := cond()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errTimeout
}

var errTimeout = errors.New("timed out")

func (u *upgrader) alive() error {
	if err := syscall.Kill(u.pid, 0); err != nil {
		return fmt.Errorf("daemon %d died during the upgrade — check its log; sessions may be lost", u.pid)
	}
	return nil
}

// serving reports whether a daemon answers the wire handshake.
// Dialing alone proves nothing: the listener outlives every child.
func (u *upgrader) serving() bool {
	conn, err := net.DialTimeout("unix", u.sock, time.Second)
	if err != nil {
		return false
	}
	c := clientproto.Wrap(conn)
	defer c.Close()
	done := make(chan error, 1)
	go func() {
		_, err := c.Hello("serve-upgrade")
		done <- err
	}()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(3 * time.Second):
		return false
	}
}

// supervised: the supervisor hands off its child and re-execs itself.
func (u *upgrader) supervised(child int) int {
	if child == 0 {
		fmt.Fprintf(os.Stderr, upgradeMsg+"supervisor %d has no daemon child right now; try again in a moment\n", u.pid)
		return 1
	}
	if err := syscall.Kill(u.pid, syscall.SIGUSR2); err != nil {
		fmt.Fprintf(os.Stderr, upgradeMsg+"signal supervisor %d: %v\n", u.pid, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, upgradeMsg+"asked supervisor %d to upgrade daemon child %d to %s\n", u.pid, child, u.target)
	var next int
	err := u.until(func() (bool, error) {
		if err := u.alive(); err != nil {
			return false, err
		}
		next = daemonChild(u.pid)
		return next > 0 && next != child && u.runs(next) && u.runs(u.pid) && u.serving(), nil
	})
	if err == nil {
		fmt.Fprintf(os.Stderr, upgradeMsg+"upgraded: supervisor %d and daemon child %d run %s%s\n", u.pid, next, u.target, u.platformNote())
		return 0
	}
	if err != errTimeout {
		fmt.Fprintln(os.Stderr, upgradeMsg+"FAILED: "+err.Error())
		return 1
	}
	next = daemonChild(u.pid)
	switch {
	case next == child:
		fmt.Fprintf(os.Stderr, upgradeMsg+"FAILED: daemon child %d was never replaced and still runs %s — the supervisor refused the upgrade; its log says why\n", child, u.describe(child))
	case next == 0:
		fmt.Fprintf(os.Stderr, upgradeMsg+"FAILED: supervisor %d has no daemon child after %s — check its log\n", u.pid, u.wait)
	default:
		fmt.Fprintf(os.Stderr, upgradeMsg+"FAILED: within %s: daemon child %d runs %s; supervisor %d runs %s\n", u.wait, next, u.describe(next), u.pid, u.describe(u.pid))
	}
	return 1
}

// oldSupervisor: a supervisor that ignores SIGUSR2. Its crash-resume
// path is the only lever: SIGKILL the child (a clean exit would make
// the supervisor stop and drop every session) and it restarts the
// child from the binary at its path, resuming the sessions.
func (u *upgrader) oldSupervisor(child int) int {
	if ok, _, err := mapsFile(child, u.id); err == nil && ok {
		fmt.Fprintf(os.Stderr, upgradeMsg+"daemon child %d already runs %s. Supervisor %d predates in-place upgrades and keeps its old code until `xerotty serve` is restarted (which loses the sessions).\n", child, u.target, u.pid)
		return 0
	}
	fmt.Fprintf(os.Stderr, upgradeMsg+"supervisor %d predates in-place upgrades (it ignores SIGUSR2).\n", u.pid)
	fmt.Fprintf(os.Stderr, upgradeMsg+"SIGKILLing its daemon child %d: the supervisor restarts it from %s and resumes every session (shells and scrollback survive; full-screen apps repaint).\n", child, u.target)
	if err := syscall.Kill(child, syscall.SIGKILL); err != nil {
		fmt.Fprintf(os.Stderr, upgradeMsg+"kill daemon child %d: %v\n", child, err)
		return 1
	}
	var next int
	err := u.until(func() (bool, error) {
		if err := u.alive(); err != nil {
			return false, err
		}
		next = daemonChild(u.pid)
		return next > 0 && next != child && u.runs(next) && u.serving(), nil
	})
	if err == nil {
		fmt.Fprintf(os.Stderr, upgradeMsg+"daemon child %d runs %s and is serving%s. Supervisor %d still runs its old code until `xerotty serve` is restarted.\n", next, u.target, u.platformNote(), u.pid)
		return 0
	}
	if err != errTimeout {
		fmt.Fprintln(os.Stderr, upgradeMsg+"FAILED: "+err.Error())
		return 1
	}
	if next = daemonChild(u.pid); next == 0 {
		fmt.Fprintf(os.Stderr, upgradeMsg+"FAILED: supervisor %d started no new daemon child within %s — check its log\n", u.pid, u.wait)
	} else {
		fmt.Fprintf(os.Stderr, upgradeMsg+"FAILED: within %s: daemon child %d runs %s\n", u.wait, next, u.describe(next))
	}
	return 1
}

// unsupervised: SIGUSR2 makes the daemon exec the new binary in place;
// that image adopts the sessions into a supervisor and starts a child.
func (u *upgrader) unsupervised() int {
	if err := syscall.Kill(u.pid, syscall.SIGUSR2); err != nil {
		fmt.Fprintf(os.Stderr, upgradeMsg+"signal daemon %d: %v\n", u.pid, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, upgradeMsg+"triggered hot upgrade of daemon %d\n", u.pid)
	var child int
	err := u.until(func() (bool, error) {
		if err := u.alive(); err != nil {
			return false, err
		}
		// The child is the proof the exec happened even when the
		// target is the binary that was already running.
		child = daemonChild(u.pid)
		return child > 0 && u.runs(u.pid) && u.runs(child) && u.serving(), nil
	})
	if err == nil {
		fmt.Fprintf(os.Stderr, upgradeMsg+"daemon %d upgraded to %s and serving%s (now supervised: daemon child %d)\n", u.pid, u.target, u.platformNote(), child)
		return 0
	}
	if err != errTimeout {
		fmt.Fprintln(os.Stderr, upgradeMsg+"FAILED: "+err.Error())
		return 1
	}
	if child = daemonChild(u.pid); child == 0 {
		fmt.Fprintf(os.Stderr, upgradeMsg+"FAILED: within %s: daemon %d runs %s and started no daemon child — check its log\n", u.wait, u.pid, u.describe(u.pid))
	} else {
		fmt.Fprintf(os.Stderr, upgradeMsg+"FAILED: within %s: daemon %d runs %s; its daemon child %d runs %s\n", u.wait, u.pid, u.describe(u.pid), child, u.describe(child))
	}
	return 1
}
