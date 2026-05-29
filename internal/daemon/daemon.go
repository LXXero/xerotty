package daemon

import (
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/protocol"
)

// Daemon is the headless terminal session host. One Daemon owns
// a unix-socket listener and a single default session (more
// sessions in a later phase). Multiple clients can attach but
// Phase 0 serializes their writes via the session mutex.
type Daemon struct {
	cfg *config.Config

	// listener for the UI/MCP protocol socket. Guarded by
	// listenerMu because Run() writes it while Stop() (possibly
	// on another goroutine, e.g. test teardown racing startup)
	// reads + closes it. stopped lets Stop() called before Run()
	// binds still tear down cleanly — Run checks it after binding.
	listenerMu sync.Mutex
	socketPath string
	listener   net.Listener
	stopped    bool

	// Sessions, keyed by name. Phase 0 only ever holds "default".
	mu       sync.Mutex
	sessions map[string]*Session

	// All currently-connected clients (wire protocol; MCP agents
	// are tracked separately). Used by MCP agent/clients + future
	// "who's attached" UI.
	clientsMu sync.Mutex
	clients   map[*clientConn]struct{}
}

// AttachedClient is a snapshot of one connected wire-protocol client.
// Mirrors what the MCP surface needs to render "who's attached".
type AttachedClient struct {
	ClientID    string
	RemoteAddr  string
	SessionName string
	JoinedUnix  int64
}

// AttachedClients returns a snapshot of all currently-connected
// wire-protocol clients. Safe to call from any goroutine.
func (d *Daemon) AttachedClients() []AttachedClient {
	d.clientsMu.Lock()
	defer d.clientsMu.Unlock()
	out := make([]AttachedClient, 0, len(d.clients))
	for c := range d.clients {
		ac := AttachedClient{
			ClientID:   c.clientID,
			RemoteAddr: c.conn.RemoteAddr().String(),
			JoinedUnix: c.joined.Unix(),
		}
		if c.session != nil {
			ac.SessionName = c.session.Name
		}
		out = append(out, ac)
	}
	return out
}

// broadcastProposals ships the current propose-mode queue to every
// connected wire client so their approval gates stay in sync.
// Called after a proposal is queued (by an MCP agent) or resolved.
func (d *Daemon) broadcastProposals() {
	sess := d.SessionByName("default")
	if sess == nil {
		return
	}
	pending := sess.PendingProposals()
	infos := make([]protocol.ProposalInfo, len(pending))
	for i, p := range pending {
		kind := "input"
		if p.IsPaste {
			kind = "paste"
		}
		infos[i] = protocol.ProposalInfo{
			Index:   uint32(i),
			TabID:   p.TabID,
			Kind:    kind,
			Preview: previewBytes(p.Bytes),
		}
	}
	d.clientsMu.Lock()
	conns := make([]*clientConn, 0, len(d.clients))
	for c := range d.clients {
		conns = append(conns, c)
	}
	d.clientsMu.Unlock()
	for _, c := range conns {
		_ = c.writeFrame(protocol.MsgProposalsChanged, &protocol.ProposalsChanged{Proposals: infos})
	}
}

// previewBytes renders queued payload bytes display-safe: control
// chars become caret/symbol notation, truncated to 60 runes so a
// pasted screenful doesn't blow up the banner.
func previewBytes(b []byte) string {
	const max = 60
	var sb []rune
	for _, r := range string(b) {
		switch {
		case r == '\r':
			sb = append(sb, '⏎')
		case r == '\n':
			sb = append(sb, '⏎')
		case r == '\t':
			sb = append(sb, '⇥')
		case r < 0x20:
			sb = append(sb, '^', rune('@'+r))
		default:
			sb = append(sb, r)
		}
		if len(sb) >= max {
			sb = append(sb, '…')
			break
		}
	}
	return string(sb)
}

// broadcastClipboardSet ships MsgClipboardSet to every connected
// client so each writes the text to its local OS clipboard. Fired
// when a PTY app issues OSC 52 set. Clipboard is session-global,
// so this isn't scoped to a tab subscription — every attached
// client gets it.
func (d *Daemon) broadcastClipboardSet(text string) {
	d.clientsMu.Lock()
	conns := make([]*clientConn, 0, len(d.clients))
	for c := range d.clients {
		conns = append(conns, c)
	}
	d.clientsMu.Unlock()
	for _, c := range conns {
		_ = c.writeFrame(protocol.MsgClipboardSet, &protocol.ClipboardSet{Text: text})
	}
}

// broadcastBell ships MsgBell to every client with a subscription
// for tabID. Wire-protocol BEL fan-out — each attached client's
// GUI (or CLI viewer) decides what to do with it (audio, visual
// flash, count in tab title, etc.).
func (d *Daemon) broadcastBell(tabID uint32) {
	d.clientsMu.Lock()
	conns := make([]*clientConn, 0, len(d.clients))
	for c := range d.clients {
		conns = append(conns, c)
	}
	d.clientsMu.Unlock()
	for _, c := range conns {
		c.subsMu.Lock()
		_, ok := c.subs[tabID]
		c.subsMu.Unlock()
		if !ok {
			continue
		}
		_ = c.writeFrame(protocol.MsgBell, &protocol.Bell{ID: tabID})
	}
}

// broadcastScrollbackCleared notifies every attached client with a
// subscription on tabID that scrollback was cleared. Resets each
// sub's lastScrollbackLen so the next publish doesn't think the
// daemon's scrollback shrank (which would silently skip new
// growth until it climbed back past the old length). Ships
// MsgScrollbackCleared so client-side scrollback mirrors drop too.
func (d *Daemon) broadcastScrollbackCleared(tabID uint32) {
	d.clientsMu.Lock()
	conns := make([]*clientConn, 0, len(d.clients))
	for c := range d.clients {
		conns = append(conns, c)
	}
	d.clientsMu.Unlock()
	for _, c := range conns {
		c.subsMu.Lock()
		sub, ok := c.subs[tabID]
		c.subsMu.Unlock()
		if !ok {
			continue
		}
		sub.lastScrollbackLen = 0
		_ = c.writeFrame(protocol.MsgScrollbackCleared, &protocol.ScrollbackCleared{ID: tabID})
	}
}

func (d *Daemon) registerClient(c *clientConn) {
	d.clientsMu.Lock()
	defer d.clientsMu.Unlock()
	if d.clients == nil {
		d.clients = make(map[*clientConn]struct{})
	}
	d.clients[c] = struct{}{}
}

func (d *Daemon) unregisterClient(c *clientConn) {
	d.clientsMu.Lock()
	defer d.clientsMu.Unlock()
	delete(d.clients, c)
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

// Config returns the daemon's config. Read-only — callers must not
// mutate. Used by the MCP server to read trust-model settings.
func (d *Daemon) Config() *config.Config { return d.cfg }

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
	if fi, err := os.Stat(d.socketPath); err == nil {
		// Only ever remove an actual socket. If something else owns
		// this path (a regular file, a directory the user pointed us
		// at by mistake), refuse rather than silently delete their
		// data on a dial failure.
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("daemon: %s exists and is not a socket; refusing to remove it", d.socketPath)
		}
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
	// Publish the listener under the lock. If Stop() already ran
	// (raced ahead of us), tear down immediately instead of
	// blocking forever in Accept.
	d.listenerMu.Lock()
	if d.stopped {
		d.listenerMu.Unlock()
		_ = ln.Close()
		_ = os.Remove(d.socketPath)
		return nil
	}
	d.listener = ln
	d.listenerMu.Unlock()
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

// ServeConn handles one preexisting connection on this daemon. Used
// by --stdio mode where the transport is stdin/stdout rather than a
// unix socket: the caller wraps those as a net.Conn (see
// protocol.StdioConn) and hands it here. Blocks until the client
// disconnects.
//
// Sessions and tabs live on the Daemon regardless of how the conn
// got here, so all the session-management invariants from Run() still
// apply — multiple ServeConn calls from different transports can
// coexist on the same daemon.
func (d *Daemon) ServeConn(conn net.Conn) {
	d.serveConn(conn)
}

// Stop closes the listener and removes the socket file. Active
// client connections continue until their peers close them; tabs
// continue running because they're owned by the session, not the
// client.
func (d *Daemon) Stop() error {
	d.listenerMu.Lock()
	d.stopped = true
	ln := d.listener
	d.listenerMu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	if d.socketPath != "" {
		_ = os.Remove(d.socketPath)
	}
	return nil
}

// SessionByName returns the named session if it exists, or nil if
// no session by that name has been created yet. Read-only — does
// not auto-create like the unexported session() does. Used by the
// MCP server which mustn't accidentally materialize sessions just
// because an agent asked for state.
func (d *Daemon) SessionByName(name string) *Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sessions[name]
}

// session returns (and creates if missing) the named session.
// Phase 0 only ever uses "default".
func (d *Daemon) session(name string) *Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.sessions[name]; ok {
		return s
	}
	s := newSession(name, d.cfg, d)
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
