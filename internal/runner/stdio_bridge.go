package runner

import (
	"fmt"
	"github.com/LXXero/xerotty/internal/daemonspawn"
	"io"
	"net"
	"os"
	"sync"
	"syscall"

	"github.com/LXXero/xerotty/internal/sockpath"
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
// starts `xerotty serve --socket <path>` through daemonspawn (same
// path as the GUI's daemonsource.EnsureLocalDaemon: outside this ssh
// session where the OS allows it) and waits for the socket to come up
// before dialing again.
func connectOrSpawnDaemon(socketPath string) (net.Conn, error) {
	// Quick try first — common case is "daemon's already running".
	if c, err := net.Dial("unix", socketPath); err == nil {
		return c, nil
	}

	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate self for auto-spawn: %w", err)
	}
	// NEVER the spawner's stderr: over ssh that's the session's pipe,
	// and the daemon would die of SIGPIPE on its first log line after
	// the session ends (see sockpath.DaemonLogFile). nil = /dev/null
	// when the log can't be opened.
	logFile := sockpath.DaemonLogFile()
	if logFile != nil {
		defer logFile.Close()
	}
	dial := func() bool {
		c, err := net.Dial("unix", socketPath)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}
	if _, err := daemonspawn.EnsureListening(daemonspawn.Options{
		Self:       self,
		SocketPath: socketPath,
		Stderr:     logFile,
		Dial:       dial,
	}); err != nil {
		return nil, fmt.Errorf("auto-spawn daemon: %w", err)
	}
	return net.Dial("unix", socketPath)
}
