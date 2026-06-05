// Hot-upgrade orchestration: the exec-in-place jump (old image) and
// the resume entry (new image). The daemon package owns WHAT
// serializes (daemon.SerializeUpgrade / ResumeFromHandoff); this
// file owns the process mechanics — quiesce ordering, FD_CLOEXEC
// clearing, argv reconstruction, syscall.Exec. See
// docs/UPGRADE_PLAN.md.

package runner

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/handoff"
	"github.com/LXXero/xerotty/internal/mcp"
)

// execUpgrade is the point of no return: serialize the session,
// surrender the terminals, and exec newBinary in this process. On
// success it never returns. The error paths are ordered so failure
// BEFORE terminals release leaves the daemon fully alive; failure
// after release (exec itself failing) is reported but sessions are
// already unservable — the caller should exit.
//
// Caller must have stopped the listeners + client connections first
// (a publishLoop snapshotting mid-release reads a dead emulator).
func execUpgrade(d *daemon.Daemon, newBinary, socketPath, mcpSocketPath string) error {
	// Resolve before touching anything — a bad path must abort with
	// the daemon intact.
	bin, err := exec.LookPath(newBinary)
	if err != nil {
		return fmt.Errorf("upgrade: resolve %q: %w", newBinary, err)
	}
	if bin, err = filepath.Abs(bin); err != nil {
		return fmt.Errorf("upgrade: abs: %w", err)
	}

	// VALIDATION GATE — runs while aborting is still free. The new
	// binary must prove it executes and speaks this handoff version
	// by validating a synthetic probe file as a child process.
	// After SerializeUpgrade the terminals are released and there is
	// no way back, so a broken/incompatible target binary has to be
	// caught HERE to mean "upgrade aborted, sessions intact" rather
	// than "sessions lost".
	probe := filepath.Join(filepath.Dir(socketPath), "xerottyd.handoff.probe")
	probeState := &handoff.State{WireListenFD: -1, MCPListenFD: -1}
	if err := probeState.WriteFile(probe); err != nil {
		return fmt.Errorf("upgrade: write probe: %w", err)
	}
	vout, verr := exec.Command(bin, "serve", "--validate-handoff", probe).CombinedOutput()
	_ = os.Remove(probe)
	if verr != nil {
		return fmt.Errorf("upgrade: validation gate: %s rejected handoff v%d (%v: %s) — aborted with sessions intact",
			bin, handoff.Version, verr, strings.TrimSpace(string(vout)))
	}

	// Point of no return starts here: terminals release inside.
	st, keepFiles, err := d.SerializeUpgrade()
	if err != nil {
		return fmt.Errorf("upgrade: serialize: %w", err)
	}
	// Pass the wire listener through the exec so the socket never
	// closes — reconnecting clients land on the new image with no
	// connection-refused window. (The MCP socket re-binds fresh in
	// the new image instead: agents reconnect per-request anyway.)
	if lf := d.ListenerFile(); lf != nil {
		st.WireListenFD = int(lf.Fd())
		keepFiles = append(keepFiles, lf)
	}

	stateFile := filepath.Join(filepath.Dir(socketPath), "xerottyd.handoff")
	if err := st.WriteFile(stateFile); err != nil {
		return fmt.Errorf("upgrade: write state: %w", err)
	}

	// Everything that must survive the exec loses FD_CLOEXEC now —
	// Go opens fds cloexec by design, survival is opt-in per fd.
	// unix.FcntlInt, not a raw syscall: raw syscalls are deprecated
	// on darwin (libc is the only supported gate there).
	for _, f := range keepFiles {
		fd := int(f.Fd())
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
			return fmt.Errorf("upgrade: clear cloexec on fd %d: %w", fd, err)
		}
	}

	argv := []string{"xerotty", "serve", "--resume", stateFile, "--socket", socketPath}
	if mcpSocketPath != "" {
		argv = append(argv, "--mcp-socket", mcpSocketPath)
	} else {
		argv = append(argv, "--no-mcp")
	}
	fmt.Fprintf(os.Stderr, "xerotty serve: exec-in-place upgrade -> %s (%d tabs)\n", bin, len(st.Tabs))
	// Exec only returns on failure. The old image (goroutines and
	// all) is otherwise gone between this line and the new main().
	err = syscall.Exec(bin, argv, os.Environ())
	// KeepAlive pins the *os.File handles until AFTER the exec
	// attempt: their finalizers close the fds at GC, and nothing
	// else references them anymore — without this pin the handoff
	// fds are dead numbers by the time the new image adopts them.
	runtime.KeepAlive(keepFiles)
	return fmt.Errorf("upgrade: exec %s: %w", bin, err)
}

// resumeFromFile is the new image's side: load + delete the state
// file, rebuild the session around the inherited fds. Runs before
// the daemon starts listening so clients reconnecting mid-resume
// can't observe a half-built session. Returns the inherited wire
// listener when the old image passed one (nil = bind fresh).
func resumeFromFile(d *daemon.Daemon, path string) (net.Listener, error) {
	st, err := handoff.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// The file holds terminal contents — delete it the moment it's
	// parsed, success or not below.
	_ = os.Remove(path)
	var ln net.Listener
	if st.WireListenFD >= 0 {
		f := os.NewFile(uintptr(st.WireListenFD), "wire-listener")
		if l, err := net.FileListener(f); err == nil {
			ln = l
			// FileListener dups the fd; drop ours.
			_ = f.Close()
		}
	}
	return ln, d.ResumeFromHandoff(st)
}

// upgradeOnSignal arms SIGUSR2 as the upgrade trigger (the nginx
// convention): on signal, stop serving and exec the binary at our
// own installed path. Phase 4 adds the `serve --upgrade` CLI
// trigger over the wire; the signal path is what it drives and is
// independently useful (`pkill -USR2 -f 'xerotty serve'` upgrades
// in place TODAY).
//
// The target binary is resolved via the PATH-installed name when
// possible — NOT /proc/self/exe, which pins the old (possibly
// deleted) inode and would "upgrade" to the same code forever.
// The returned channel closes the moment an upgrade begins — the
// serve main loop must PARK on it after Run returns instead of
// exiting, because quiesce stops the listener (which gracefully
// ends Run) while this goroutine is still mid-flight to the exec.
// Without the park, main exits and takes the upgrade down with it.
func upgradeOnSignal(d *daemon.Daemon, mcpSrv *mcp.Server, socketPath, mcpSocketPath string) <-chan struct{} {
	upgrading := make(chan struct{})
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR2)
	go func() {
		for range ch {
			target := upgradeTargetBinary()
			fmt.Fprintf(os.Stderr, "xerotty serve: SIGUSR2 — upgrading to %s\n", target)
			close(upgrading)
			// Quiesce: no new clients, no publishers mid-release.
			// Step logs are deliberate — if an upgrade ever wedges,
			// the last line names the stuck step. The wire listener
			// is SUSPENDED, not stopped — its fd survives the exec
			// so the socket never closes.
			if mcpSrv != nil {
				fmt.Fprintln(os.Stderr, "xerotty serve: upgrade: stopping mcp")
				_ = mcpSrv.Stop()
			}
			fmt.Fprintln(os.Stderr, "xerotty serve: upgrade: suspending listener")
			d.Suspend()
			fmt.Fprintln(os.Stderr, "xerotty serve: upgrade: disconnecting clients")
			d.DisconnectClients()
			fmt.Fprintln(os.Stderr, "xerotty serve: upgrade: serializing")
			if err := execUpgrade(d, target, socketPath, mcpSocketPath); err != nil {
				// Past Stop() the daemon can't serve anymore; if the
				// terminals were released the sessions are gone too.
				// Exiting beats running on as a zombie.
				fmt.Fprintf(os.Stderr, "xerotty serve: upgrade failed: %v\n", err)
				os.Exit(1)
			}
		}
	}()
	return upgrading
}

// upgradeCLI implements `xerotty serve --upgrade`: find the running
// daemon through its socket (SO_PEERCRED — no pidfile to go stale),
// send SIGUSR2, and confirm the daemon is still answering after.
// The daemon does the rest (see upgradeOnSignal).
func upgradeCLI(socketPath string) int {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "xerotty serve --upgrade: no daemon on %s: %v\n", socketPath, err)
		return 1
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		conn.Close()
		fmt.Fprintln(os.Stderr, "xerotty serve --upgrade: not a unix socket connection")
		return 1
	}
	pid, err := peerPID(uc)
	conn.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "xerotty serve --upgrade: peer pid: %v\n", err)
		return 1
	}
	if err := syscall.Kill(pid, syscall.SIGUSR2); err != nil {
		fmt.Fprintf(os.Stderr, "xerotty serve --upgrade: signal daemon %d: %v\n", pid, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "xerotty serve --upgrade: triggered hot upgrade of daemon %d\n", pid)
	// Confirm the (same-pid) daemon is serving again. The listener
	// fd passes through the exec so dial keeps succeeding; the
	// meaningful check is that the PROCESS survived — exec failure
	// exits it.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			fmt.Fprintln(os.Stderr, "xerotty serve --upgrade: daemon died during upgrade — check its log; sessions may be lost")
			return 1
		}
		if c, err := net.Dial("unix", socketPath); err == nil {
			c.Close()
			time.Sleep(300 * time.Millisecond) // let the exec land
			if err := syscall.Kill(pid, 0); err == nil {
				fmt.Fprintf(os.Stderr, "xerotty serve --upgrade: daemon %d upgraded and serving\n", pid)
				return 0
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "xerotty serve --upgrade: timed out confirming; check the daemon log")
	return 1
}

// upgradeTargetBinary picks what to exec, in order:
//
//  1. $XEROTTY_UPGRADE_BINARY — explicit override (tests, unusual
//     installs).
//  2. The path we were STARTED from, re-stat'd: after an install
//     replaces the file, this is the new binary at the old path.
//     /proc/self/exe pins the old inode (kernel appends
//     " (deleted)" once it's unlinked) — strip that and use the
//     path, not the inode.
//  3. PATH-resolved "xerotty".
func upgradeTargetBinary() string {
	if p := os.Getenv("XEROTTY_UPGRADE_BINARY"); p != "" {
		return p
	}
	if self, err := os.Executable(); err == nil {
		self = strings.TrimSuffix(self, " (deleted)")
		if _, err := os.Stat(self); err == nil {
			return self
		}
	}
	if p, err := exec.LookPath("xerotty"); err == nil {
		return p
	}
	return "xerotty"
}
