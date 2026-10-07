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
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/handoff"
	"github.com/LXXero/xerotty/internal/mcp"
	"github.com/LXXero/xerotty/internal/supervise"
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

	// argv[0] is the absolute path: macOS ps reports argv[0] as comm,
	// and `serve --upgrade` reads the daemon's binary from there.
	argv := []string{bin, "serve", "--resume", stateFile, "--socket", socketPath}
	if mcpSocketPath != "" {
		argv = append(argv, "--mcp-socket", mcpSocketPath)
	} else {
		argv = append(argv, "--no-mcp")
	}
	// A supervised child stays supervised across the exec: the control
	// channel rides through as a non-cloexec fd, and the new image
	// re-attaches to the same supervisor.
	if cfd := d.SupervisorExecFD(); cfd >= 0 {
		argv = append(argv, "--child", "--control-fd", strconv.Itoa(cfd))
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

// upgradeOnSignal arms the two upgrade triggers and returns a channel
// that closes the moment an upgrade begins — the serve main loop must
// PARK on it after Run returns instead of exiting, because quiesce
// stops the listener (which gracefully ends Run) while the upgrade
// goroutine is still mid-flight. Without the park, main exits and
// takes the upgrade down with it.
//
//   - SIGUSR2 (the nginx convention): exec the binary at our own
//     installed path in place. Under a supervisor that announced it
//     handles upgrades itself (CapsMsg), the supervisor owns the
//     signal and we ignore it here — a pkill that hits both processes
//     must not upgrade twice.
//   - An UpgradeMsg from the supervisor: hand off and exit, and the
//     supervisor starts the new binary from our handoff.
//
// The target binary is resolved via the PATH-installed name when
// possible — NOT /proc/self/exe, which pins the old (possibly
// deleted) inode and would "upgrade" to the same code forever.
func upgradeOnSignal(d *daemon.Daemon, mcpSrv *mcp.Server, sup *supervise.Client, socketPath, mcpSocketPath string) <-chan struct{} {
	upgrading := make(chan struct{})
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR2)
	var requests <-chan supervise.UpgradeMsg
	if sup != nil {
		requests = sup.UpgradeRequests()
	}
	go func() {
		for {
			select {
			case <-ch:
				if os.Getenv("XEROTTY_TEST_IGNORE_SIGUSR2") != "" {
					// Test hook: a child that never answers SIGUSR2, for
					// `serve --upgrade`'s SIGKILL fallback.
					fmt.Fprintln(os.Stderr, "xerotty serve: SIGUSR2 ignored (XEROTTY_TEST_IGNORE_SIGUSR2)")
					continue
				}
				if sup != nil && sup.SupervisorUpgrades() {
					fmt.Fprintf(os.Stderr, "xerotty serve: SIGUSR2 ignored: the supervisor (pid %d) runs upgrades\n", os.Getppid())
					continue
				}
				target := upgradeTargetBinary()
				fmt.Fprintf(os.Stderr, "xerotty serve: SIGUSR2 — upgrading to %s\n", target)
				close(upgrading)
				quiesce(d, mcpSrv)
				if err := execUpgrade(d, target, socketPath, mcpSocketPath); err != nil {
					// Past Stop() the daemon can't serve anymore; if the
					// terminals were released the sessions are gone too.
					// Exiting beats running on as a zombie.
					fmt.Fprintf(os.Stderr, "xerotty serve: upgrade failed: %v\n", err)
					os.Exit(1)
				}
			case req := <-requests:
				fmt.Fprintln(os.Stderr, "xerotty serve: supervisor asked for an upgrade handoff")
				close(upgrading)
				quiesce(d, mcpSrv)
				os.Exit(handOffToSupervisor(d, req.Handoff))
			}
		}
	}()
	return upgrading
}

// quiesce stops serving before a handoff: no new clients, no
// publishers mid-release. Step logs are deliberate — if an upgrade
// ever wedges, the last line names the stuck step. The wire listener
// is SUSPENDED, not stopped — its fd must outlive this process image.
func quiesce(d *daemon.Daemon, mcpSrv *mcp.Server) {
	if mcpSrv != nil {
		fmt.Fprintln(os.Stderr, "xerotty serve: upgrade: stopping mcp")
		_ = mcpSrv.Stop()
	}
	fmt.Fprintln(os.Stderr, "xerotty serve: upgrade: suspending listener")
	d.Suspend()
	fmt.Fprintln(os.Stderr, "xerotty serve: upgrade: disconnecting clients")
	d.DisconnectClients()
	fmt.Fprintln(os.Stderr, "xerotty serve: upgrade: serializing")
}

// handOffToSupervisor writes the full session state for the
// supervisor and returns the exit status to leave with. The fd
// numbers in the file are ours and die with us: the supervisor has
// held its own copy of every PTY master and scrollback file since
// each tab spawned, and keeps the listener. Any failure exits
// non-zero, which the supervisor treats as a crash and resumes from
// the topology we streamed.
func handOffToSupervisor(d *daemon.Daemon, path string) int {
	st, _, err := d.SerializeUpgrade()
	if err != nil {
		fmt.Fprintf(os.Stderr, "xerotty serve: upgrade handoff: %v\n", err)
		return 1
	}
	if err := st.WriteFile(path); err != nil {
		fmt.Fprintf(os.Stderr, "xerotty serve: upgrade handoff: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "xerotty serve: upgrade handoff written (%d tabs); exiting for the supervisor\n", len(st.Tabs))
	return supervise.ExitUpgrade
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
