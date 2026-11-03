package daemonsource

import (
	"fmt"
	"net"
	"os"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/daemonspawn"
	"github.com/LXXero/xerotty/internal/sockpath"
)

// EnsureLocalDaemon makes sure a local xerotty daemon is reachable
// on socketPath. If something is already listening there, returns
// a connected Client. Otherwise it starts `xerotty serve --socket
// <path>` through daemonspawn — outside this process's login session
// where the OS allows (systemd user scope, launchd gui domain), a new
// session via fork elsewhere — waits for the socket to bind, then
// connects. See package daemonspawn for why placement matters.
//
// socketPath: pass "" to use the default ($XDG_RUNTIME_DIR/
// xerottyd.sock, or /tmp/xerottyd-<uid>.sock).
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

	// The daemon must outlive the GUI — including the GUI's stderr.
	// Inheriting it means a daemon whose spawning terminal/session
	// closes dies of SIGPIPE on its next log line (Go re-raises
	// EPIPE on fds 1/2). Log to the canonical file instead.
	logFile := sockpath.DaemonLogFile()
	if logFile != nil {
		defer logFile.Close()
	}
	if _, err := daemonspawn.EnsureListening(daemonspawn.Options{
		Self:       self,
		SocketPath: socketPath,
		Stderr:     logFile,
		Dial:       func() bool { return SocketAlive(socketPath) },
	}); err != nil {
		return nil, fmt.Errorf("ensure daemon: %w", err)
	}
	return clientproto.Dial(socketPath)
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
