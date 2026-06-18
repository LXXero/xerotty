package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/protocol"
	"github.com/LXXero/xerotty/internal/terminal"
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
	suspended atomic.Bool // upgrade quiesce: refuse new conns, keep listener

	// Heartbeat tuning (Phase 10 layer 2). Defaults set in New;
	// SetHeartbeat overrides them (tests use short windows). Read by
	// each conn's heartbeatLoop + the handshake watchdog.
	heartbeatInterval time.Duration
	// livenessWindow is the staleness threshold: a conn is reaped only
	// when BOTH write-progress and pong have been stale this long. Also
	// bounds the synchronous handshake (watchdog).
	livenessWindow time.Duration

	// instanceID is a random nonce minted once at New() — the identity
	// of THIS daemon process's tab-id space. Shipped in every Attached
	// so clients can tell a reconnect-to-same-daemon (same ID) from a
	// reconnect-after-restart (new ID) and scope close-tombstones to it.
	instanceID string
}

// SetHeartbeat overrides the heartbeat ping interval and the liveness
// staleness window (also the handshake watchdog). Call before Run /
// serving connections (new conns read these). Used by tests to shorten
// the windows.
func (d *Daemon) SetHeartbeat(interval, window time.Duration) {
	d.heartbeatInterval = interval
	d.livenessWindow = window
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
		// Read the session name from the atomic mirror — c.session
		// itself is owned by the read loop and racy to touch here.
		if v, ok := c.sessionName.Load().(string); ok {
			ac.SessionName = v
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
		c.sendProposals(&protocol.ProposalsChanged{Proposals: infos})
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
		c.trySend(protocol.MsgClipboardSet, &protocol.ClipboardSet{Text: text})
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
		c.trySend(protocol.MsgBell, &protocol.Bell{ID: tabID})
	}
}

// broadcastScrollbackCleared notifies every attached client with a
// subscription on tabID that scrollback was cleared. It flags each
// sub and wakes its publish goroutine, which resets lastScrollbackLen
// (so the next publish doesn't think the daemon's scrollback shrank
// and skip new growth) and ships MsgScrollbackCleared so client-side
// mirrors drop too — both done on the publish goroutine to stay
// ordered against in-flight appends.
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
		// Hand the reset + MsgScrollbackCleared to the sub's publish
		// goroutine (via clearPending + wake) so they're serialized
		// against any in-flight scrollback append. Doing the reset
		// here would race a publish that already snapshotted rows and
		// is about to store a stale lastScrollbackLen / ship a stale
		// append, repopulating the history we just cleared.
		sub.clearPending.Store(true)
		select {
		case sub.wake <- struct{}{}:
		default:
		}
	}
}

// --- topology mutation funnel ---
//
// CreateTab / CloseTab / MoveTab / CreateWindow / CloseWindow are the
// single entry points for structural changes. Each mutates the
// Session (which bumps its revision) and then broadcasts the new
// topology snapshot to every client attached to that session. BOTH
// the wire handlers and the MCP server route through these so an
// MCP-driven create/close is seen by a watching GUI, not just the
// agent that made it. Callers must not poke Session.NewTab/CloseTab/
// etc. directly when they want the change broadcast.

// CreateTab spawns a tab in the session and broadcasts the topology.
// It subscribes EVERY client currently attached to the session to the
// new tab — not just the requester — so a tab created by any client
// (or an MCP agent) has a daemon-side publish loop feeding cells /
// title / state / scrollback to all of them. Without this, other
// clients adopt the tab from the broadcast but see it blank.
func (d *Daemon) CreateTab(sess *Session, windowID uint32, cols, rows int, cwd string) (*Tab, *Window, error) {
	t, w, _, err := d.CreateNamedTab(sess, "", windowID, cols, rows, cwd, nil)
	return t, w, err
}

// CreateNamedTab is CreateTab with an idempotency label: if `name`
// already tags a live tab in the session, it returns that tab
// (created=false) instead of spawning a duplicate. The subscribe +
// topology broadcast only fire for a genuinely new tab — reusing an
// existing one needs neither, since every client is already
// subscribed and the topology hasn't changed. name == "" is the
// plain always-spawn path.
func (d *Daemon) CreateNamedTab(sess *Session, name string, windowID uint32, cols, rows int, cwd string, launch *terminal.LaunchCmd) (*Tab, *Window, bool, error) {
	t, w, created, err := sess.FindOrCreateTab(name, windowID, cols, rows, cwd, launch)
	if err != nil {
		return nil, nil, false, err
	}
	if created {
		d.subscribeSessionClients(sess, t)
		d.broadcastTopology(sess)
	}
	return t, w, created, nil
}

// CloseTab removes a tab from the session and broadcasts the topology.
// It tears down the tab's subscription on EVERY client first — publish
// loops outlive child exit now (M2), so a missed unsubscribe would
// leak a goroutine plus a terminal ref on every viewer, not just the
// requester.
func (d *Daemon) CloseTab(sess *Session, id uint32) {
	d.unsubscribeSessionClients(sess, id)
	sess.CloseTab(id)
	d.broadcastTopology(sess)
}

// MoveTab moves a tab between windows and broadcasts the topology.
func (d *Daemon) MoveTab(sess *Session, tabID, toWindowID uint32, index int) {
	sess.MoveTab(tabID, toWindowID, index)
	d.broadcastTopology(sess)
}

// CreateWindow registers a window in the session and broadcasts.
func (d *Daemon) CreateWindow(sess *Session, posX, posY, width, height int32) *Window {
	w := sess.NewWindow(posX, posY, width, height)
	d.broadcastTopology(sess)
	return w
}

// CloseWindow tears down a window and broadcasts the topology.
func (d *Daemon) CloseWindow(sess *Session, id uint32) {
	sess.CloseWindow(id)
	d.broadcastTopology(sess)
}

// broadcastTopology ships the session's current topology snapshot to
// every client attached to that session. Snapshot + revision (not
// deltas) so clients self-heal: a client applies it only if the
// revision is newer, replacing its view wholesale — no delta-ordering
// bug class. Clients attached to other sessions (or not yet attached)
// are skipped via the sessionName atomic mirror, which is safe to read
// off the read-loop goroutine (unlike c.session).
func (d *Daemon) broadcastTopology(sess *Session) {
	snap := sess.TopologySnapshot()
	d.clientsMu.Lock()
	conns := make([]*clientConn, 0, len(d.clients))
	for c := range d.clients {
		conns = append(conns, c)
	}
	d.clientsMu.Unlock()
	for _, c := range conns {
		if name, _ := c.sessionName.Load().(string); name != sess.Name {
			continue
		}
		c.sendTopology(&snap)
	}
}

// subscribeSessionClients starts a publish loop for tab t on every
// client attached to sess. subscribe is idempotent (a no-op if the
// client already has the sub), so re-subscribing the requesting client
// is harmless. Tab IDs are per-session, so the sessionName filter is
// required — a same-numbered tab in another session must not be touched.
func (d *Daemon) subscribeSessionClients(sess *Session, t *Tab) {
	for _, c := range d.sessionClients(sess) {
		// subscribe re-checks (under subsMu) that c is still attached
		// to sess, so a connection detaching/disconnecting concurrently
		// with this fan-out won't get a leaked publish loop.
		c.subscribe(sess, t)
	}
}

// unsubscribeSessionClients tears down tab id's publish loop on every
// client attached to sess (no-op for clients without that sub).
func (d *Daemon) unsubscribeSessionClients(sess *Session, id uint32) {
	for _, c := range d.sessionClients(sess) {
		c.unsubscribe(id)
	}
}

// sessionClients snapshots the client connections attached to sess
// (matched via the sessionName atomic mirror, safe off the read-loop
// goroutine).
func (d *Daemon) sessionClients(sess *Session) []*clientConn {
	d.clientsMu.Lock()
	defer d.clientsMu.Unlock()
	out := make([]*clientConn, 0, len(d.clients))
	for c := range d.clients {
		if name, _ := c.sessionName.Load().(string); name == sess.Name {
			out = append(out, c)
		}
	}
	return out
}

// reconcileTabSize sizes the tab's shared PTY to the SMALLEST grid any
// attached client wants, so multiple differently-sized GUIs share one
// PTY without a resize war. Each client's frame loop continuously
// demands the grid match its OWN window (app.resizeReq); obeying the
// last requester unconditionally made two clients ping-pong the grid
// between their sizes ~2Hz — the flashing. Reconciling to the min lets
// every viewer see the whole grid (the larger ones letterbox, like
// tmux), and we only Resize + wake when the reconciled size actually
// CHANGES, so the larger client's futile re-requests become no-ops
// instead of fuel. Called on every client resize request and on
// detach (when a small client leaves, the min can grow back).
//
// Clients that haven't expressed a size yet (desired == 0) don't
// constrain; if NONE has, the PTY is left as-is.
func (d *Daemon) reconcileTabSize(sess *Session, tabID uint32) {
	if sess == nil {
		return
	}
	t := sess.Tab(tabID)
	if t == nil {
		return
	}
	d.clientsMu.Lock()
	conns := make([]*clientConn, 0, len(d.clients))
	for c := range d.clients {
		conns = append(conns, c)
	}
	d.clientsMu.Unlock()

	minCols, minRows := 0, 0
	for _, c := range conns {
		c.subsMu.Lock()
		sub, ok := c.subs[tabID]
		var dc, dr uint16
		if ok {
			dc, dr = sub.desiredCols, sub.desiredRows
		}
		c.subsMu.Unlock()
		if !ok || dc == 0 || dr == 0 {
			continue
		}
		if minCols == 0 || int(dc) < minCols {
			minCols = int(dc)
		}
		if minRows == 0 || int(dr) < minRows {
			minRows = int(dr)
		}
	}
	if minCols == 0 || minRows == 0 {
		return // no attached client has expressed a size yet
	}
	minCols = ClampTabDim(minCols)
	minRows = ClampTabDim(minRows)
	if t.Term.Width() == minCols && t.Term.Height() == minRows {
		return // already at the reconciled size — no thrash, no broadcast
	}
	t.Term.Resize(minCols, minRows)
	d.WakeTabSubscribers(tabID)
}

// WakeTabSubscribers nudges every client's publishLoop subscribed to
// tabID to re-publish immediately. Used after a resize: it changes
// the grid dimensions but emits no PTY output, so without an explicit
// nudge clients would only re-paint on the 500ms fallback tick. The
// shared Terminal.DataCh can't do this — a single token there wakes
// just one of the competing publishLoops, leaving the other attached
// clients showing stale dimensions. Per-sub wake fans out to all.
func (d *Daemon) WakeTabSubscribers(tabID uint32) {
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
		select {
		case sub.wake <- struct{}{}:
		default:
		}
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
		cfg:               cfg,
		socketPath:        socketPath,
		sessions:          make(map[string]*Session),
		heartbeatInterval: defaultHeartbeatInterval,
		livenessWindow:    defaultLivenessWindow,
		instanceID:        newInstanceID(),
	}
}

// newInstanceID mints a random per-process identity for the daemon's
// tab-id space. crypto/rand so two daemons (or a restart) never collide;
// a collision would let a client keep stale tombstones across a restart.
// Falls back to a pid+time string only if the OS RNG is unavailable
// (effectively never) — still distinct enough across a restart.
func newInstanceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("pid%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
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
	// Filesystem-perm gate: only this user can connect.
	_ = os.Chmod(d.socketPath, 0o600)
	return d.RunWithListener(ln)
}

// RunWithListener serves on an already-bound listener. The hot
// upgrade's resume path uses this with the listener fd inherited
// through the exec, so the socket never closes and reconnecting
// clients never see a connection-refused window.
func (d *Daemon) RunWithListener(ln net.Listener) error {
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
		if d.suspended.Load() {
			// Upgrade in flight: the session is being serialized.
			// Refuse instantly; the client's reconnect loop lands
			// on the NEW image via the inherited listener.
			_ = conn.Close()
			continue
		}
		go d.serveConn(conn)
	}
}

// Suspend makes the daemon refuse new connections without closing
// the listener — upgrade quiesce. The listener itself survives the
// exec (its fd passes through), so suspension here means "the next
// image will take your call".
func (d *Daemon) Suspend() { d.suspended.Store(true) }

// ListenerFile dups the wire listener for handoff. Returns nil if
// the daemon isn't serving on a unix listener.
func (d *Daemon) ListenerFile() *os.File {
	d.listenerMu.Lock()
	ln := d.listener
	d.listenerMu.Unlock()
	ul, ok := ln.(*net.UnixListener)
	if !ok || ul == nil {
		return nil
	}
	f, err := ul.File()
	if err != nil {
		return nil
	}
	return f
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
