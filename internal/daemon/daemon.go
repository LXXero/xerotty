package daemon

import (
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/LXXero/xerotty/internal/config"
)

// Daemon is the headless terminal session host. One Daemon owns
// a unix-socket listener and a single default session (more
// sessions in a later phase). Multiple clients can attach but
// Phase 0 serializes their writes via the session mutex.
type Daemon struct {
	cfg *config.Config

	// listener for the UI/MCP protocol socket.
	socketPath string
	listener   net.Listener

	// Sessions, keyed by name. Phase 0 only ever holds "default".
	mu       sync.Mutex
	sessions map[string]*Session
}

// New constructs a daemon configured to listen on socketPath. The
// path is created when Run is called; pre-existing files are not
// overwritten (an existing socket means another daemon is already
// listening — caller must remove it first if it's stale).
func New(cfg *config.Config, socketPath string) *Daemon {
	return &Daemon{
		cfg:        cfg,
		socketPath: socketPath,
		sessions:   make(map[string]*Session),
	}
}

// SocketPath returns the unix-socket path the daemon listens on.
// Useful for the auto-spawn flow where the UI forks the daemon and
// needs to know where to connect (the daemon prints it on stdout
// at startup; this is the canonical value).
func (d *Daemon) SocketPath() string { return d.socketPath }

// Run blocks until the listener errors out or Stop is called from
// another goroutine. The default session is created lazily on the
// first Attach.
func (d *Daemon) Run() error {
	// If a stale socket file exists and nobody's listening on it,
	// remove and retry. Real "another daemon already running"
	// would manifest as a connect-succeeds probe; we don't do
	// that here because callers know whether they expect to be
	// the first daemon. Phase 0 just refuses if the file exists.
	if _, err := os.Stat(d.socketPath); err == nil {
		// Try to connect — if successful, another daemon is live
		// and we bail. If it fails, the socket is stale and we
		// can take over.
		if c, err := net.Dial("unix", d.socketPath); err == nil {
			c.Close()
			return fmt.Errorf("daemon: socket %s is already in use by another xerottyd", d.socketPath)
		}
		_ = os.Remove(d.socketPath)
	}

	ln, err := net.Listen("unix", d.socketPath)
	if err != nil {
		return fmt.Errorf("daemon: listen %s: %w", d.socketPath, err)
	}
	d.listener = ln
	// Filesystem-perm gate: only this user can connect.
	_ = os.Chmod(d.socketPath, 0o600)

	for {
		conn, err := ln.Accept()
		if err != nil {
			// Stop closes the listener which surfaces here as an
			// error — treat that as graceful shutdown.
			if isClosedErr(err) {
				return nil
			}
			return fmt.Errorf("daemon: accept: %w", err)
		}
		go d.serveConn(conn)
	}
}

// Stop closes the listener and removes the socket file. Active
// client connections continue until their peers close them; tabs
// continue running because they're owned by the session, not the
// client.
func (d *Daemon) Stop() error {
	if d.listener != nil {
		_ = d.listener.Close()
	}
	if d.socketPath != "" {
		_ = os.Remove(d.socketPath)
	}
	return nil
}

// session returns (and creates if missing) the named session.
// Phase 0 only ever uses "default".
func (d *Daemon) session(name string) *Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.sessions[name]; ok {
		return s
	}
	s := newSession(name, d.cfg)
	d.sessions[name] = s
	return s
}

// isClosedErr matches net.ErrClosed and the variants the listener
// may return when Stop closes it under us.
func isClosedErr(err error) bool {
	return err != nil && (err == net.ErrClosed ||
		// Older Go versions / different platforms phrase it
		// differently. Substring match as a fallback.
		errContains(err, "use of closed network connection"))
}

func errContains(err error, sub string) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for i := 0; i+len(sub) <= len(msg); i++ {
		if msg[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
