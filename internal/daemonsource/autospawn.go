package daemonsource

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/sockpath"
)

// EnsureLocalDaemon makes sure a local xerotty daemon is reachable
// on socketPath. If something is already listening there, returns
// a connected Client. Otherwise it forks `xerotty serve --socket
// <path>` in the background and waits up to ~2 seconds for the
// socket to bind, then connects.
//
// socketPath: pass "" to use the default ($XDG_RUNTIME_DIR/
// xerottyd.sock, or /tmp/xerottyd-<uid>.sock).
//
// The forked daemon survives the calling process exiting (detached
// with setpgid so it doesn't get a SIGHUP from the GUI's tty/
// session). That's intentional — the whole point of daemon mode is
// that tabs outlive the GUI.
func EnsureLocalDaemon(socketPath string) (*clientproto.Client, error) {
	if socketPath == "" {
		socketPath = DefaultSocketPath()
	}
	// Already up?
	if c, err := clientproto.Dial(socketPath); err == nil {
		return c, nil
	}

	// Look up our own executable so we can re-exec it as the
	// daemon (one binary, two roles).
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("ensure daemon: locate self: %w", err)
	}

	cmd := exec.Command(self, "serve", "--socket", socketPath)
	// Don't tie the daemon to our process group; we want it to
	// outlive the GUI. Setpgid + nil-out stdio.
	cmd.SysProcAttr = detachSysProcAttr()
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = os.Stderr // surface daemon log lines so failures are visible
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ensure daemon: start: %w", err)
	}
	// Don't reap — the daemon is now independent. Calling
	// cmd.Wait() would block the GUI; release it instead.
	go func() { _ = cmd.Wait() }()

	// Poll the socket for up to ~2s. Daemon usually binds within
	// a few hundred ms; we add slack for slow boxes.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := clientproto.Dial(socketPath); err == nil {
			return c, nil
		}
		time.Sleep(40 * time.Millisecond)
	}
	return nil, fmt.Errorf("ensure daemon: forked xerotty serve but socket %s never came up", socketPath)
}

// DefaultSocketPath is the local daemon's default socket. Thin
// wrapper over sockpath (the single source of truth — see that
// package for why this must never be hand-derived per-package).
func DefaultSocketPath() string {
	return sockpath.DaemonSocket()
}

// SocketAlive returns true if something is listening on the path.
// Cheaper than EnsureLocalDaemon when you just want to probe.
func SocketAlive(path string) bool {
	c, err := net.Dial("unix", path)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
