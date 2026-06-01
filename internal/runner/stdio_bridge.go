package runner

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// runStdioBridge proxies the calling process's stdin/stdout to a
// persistent daemon on `socketPath`. Auto-spawns "xerotty serve"
// (this same binary) if nothing is listening on the socket.
//
// Used by `xerotty serve --stdio`. The win over the old "ephemeral
// in-process daemon" mode: when the user disconnects (SSH session
// ends, etc.), only the bridge dies — the daemon keeps every PTY
// alive, so the next SSH+attach finds the same tabs.
//
// Bidirectional io.Copy with a single shared shutdown channel. The
// first copy to return closes the daemon conn (which kicks the
// other copy out of its Read), then we wait for both before
// returning so we don't leak goroutines.
func runStdioBridge(socketPath string) int {
	conn, err := connectOrSpawnDaemon(socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "xerotty serve --stdio: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "xerotty serve --stdio: bridged to %s\n", socketPath)
	defer conn.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(conn, os.Stdin)
		// stdin EOF (caller disconnected) → close daemon conn so
		// the other copy returns + the daemon sees EOF too.
		_ = conn.Close()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(os.Stdout, conn)
		// daemon closed → close our stdin to unblock the other
		// copy. There's no portable way to close stdin from inside
		// a Go process; settle for sending ourselves SIGPIPE so
		// the Read returns. Cheap workaround: we'll return below
		// once both copies are done; the syscall keeps us tidy.
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGPIPE)
	}()
	wg.Wait()
	fmt.Fprintln(os.Stderr, "xerotty serve --stdio: client disconnected (daemon still running)")
	return 0
}

// connectOrSpawnDaemon dials the socket; if no daemon is there,
// forks `xerotty serve --socket <path>` detached and waits for the
// socket to come up before dialing again.
func connectOrSpawnDaemon(socketPath string) (net.Conn, error) {
	// Quick try first — common case is "daemon's already running".
	if c, err := net.Dial("unix", socketPath); err == nil {
		return c, nil
	}

	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate self for auto-spawn: %w", err)
	}
	cmd := exec.Command(self, "serve", "--socket", socketPath)
	// Detach from this process group so the daemon survives our
	// SSH session ending. Same setup as the GUI's
	// daemonsource.EnsureLocalDaemon path.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = os.Stderr // surface daemon log lines for debugging
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("auto-spawn daemon: %w", err)
	}
	go func() { _ = cmd.Wait() }() // reap

	// Poll the socket. Daemons normally bind in well under a
	// second; 2s ceiling absorbs slow remotes (e.g. raspberry pi,
	// loaded VMs).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", socketPath); err == nil {
			return c, nil
		}
		time.Sleep(40 * time.Millisecond)
	}
	return nil, fmt.Errorf("auto-spawned daemon never bound to %s", socketPath)
}
