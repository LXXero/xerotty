package daemon

import (
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LXXero/xerotty/internal/protocol"
)

// serveConn handles one client connection from accept to close. It
// runs in its own goroutine. Two sub-goroutines handle the read
// loop (client commands) and the publish loop (cell updates from
// attached tabs).
func (d *Daemon) serveConn(conn net.Conn) {
	defer conn.Close()
	c := &clientConn{
		daemon:  d,
		conn:    conn,
		reader:  protocol.NewFrameReader(conn),
		joined:  time.Now(),
	}
	// Install the unregister before handshake: handshake registers the
	// client on the daemon BEFORE writing HelloAck, so a failed ack
	// write would otherwise leak the client entry. unregisterClient is
	// a no-op delete if registration never happened (version mismatch
	// bails before registering).
	defer d.unregisterClient(c)
	if err := c.handshake(); err != nil {
		fmt.Fprintf(os.Stderr, "xerottyd: handshake failed: %v\n", err)
		return
	}
	c.runReadLoop()
}

// clientConn is the per-connection state. The read loop owns
// dispatch; the publish loop (started after Attach) owns sending
// cell frames for attached tabs.
type clientConn struct {
	daemon *Daemon
	conn   net.Conn
	reader *protocol.FrameReader

	writeMu sync.Mutex // serializes WriteFrame across goroutines

	clientID string    // from Hello
	joined   time.Time // when this conn was accepted

	session *Session // nil until Attach succeeds; only the read loop touches it

	// sessionName mirrors session.Name ("" when detached) for
	// cross-goroutine reads. AttachedClients (called on the MCP
	// goroutine) needs the name but must not touch c.session, which
	// the read loop mutates without a lock. Always holds a string.
	sessionName atomic.Value

	// Subscriptions: per attached tab, a publish goroutine + the
	// state it needs (cancel channel + last-shipped grid for diff
	// computation).
	//
	// subSession is the session these subs belong to, guarded by the
	// SAME subsMu as the sub map. It's the synchronization point for
	// the cross-connection subscribe fan-out: subscribeSessionClients
	// (running on the CREATING connection's goroutine) and this
	// connection's own teardown (stopAllSubs, on detach/disconnect)
	// both take subsMu, so a subscribe can't slip in AFTER teardown
	// and leak a publish loop. nil = not attached / torn down → don't
	// accept subscriptions.
	subsMu     sync.Mutex
	subs       map[uint32]*tabSub
	subSession *Session

	// imgAccum reassembles chunked image pastes, keyed by tab ID.
	// Chunks arrive in order on this (single) connection; on the
	// Final chunk the accumulated bytes get written + the path
	// typed. Only the read loop touches this, so no lock needed.
	imgAccum map[uint32]*imageAssembly
}

// maxConcurrentImageAssemblies caps how many in-progress chunked
// image pastes one connection may have open at once. Each assembly
// can buffer up to the 256MiB per-assembly ceiling, so without this
// a client could pin N×256MiB by interleaving uploads under distinct
// tab IDs. A real client pastes one image at a time; a few in flight
// is generous.
const maxConcurrentImageAssemblies = 4

// imageAssembly buffers an in-progress chunked image paste.
type imageAssembly struct {
	mime     string
	filename string
	buf      []byte
	nextSeq  uint32
}

type tabSub struct {
	cancel chan struct{}

	// wake nudges this subscription's publishLoop to re-publish now,
	// independent of the shared Terminal.DataCh. DataCh is a single
	// cap-1 channel every client's publishLoop competes for, so a
	// resize token on it wakes only ONE subscriber; events that must
	// reach every attached client (resize repaint, scrollback clear)
	// signal each sub's own wake instead. cap 1 — coalescing is fine.
	wake chan struct{}

	// clearPending is set by broadcastScrollbackCleared (off the
	// publish goroutine) and consumed by sendNewScrollback ON the
	// publish goroutine. Routing the reset + MsgScrollbackCleared send
	// through the publish goroutine serializes it against in-flight
	// scrollback appends, so a stale append can't repopulate cleared
	// history or leave lastScrollbackLen pointing past a cleared ring.
	clearPending atomic.Bool

	// lastGrid is what we last shipped to this client for this
	// tab. Used to compute CellDiff frames — only cells whose
	// contents/style/width changed since last send go on the wire.
	// nil until the first CellFull has been sent. Sized as
	// rows × cols; resize invalidates and forces a fresh CellFull.
	lastGrid [][]protocol.Cell
	lastCols uint16
	lastRows uint16

	// lastScrollbackLen is the highest absolute scrollback index we
	// shipped to this client. When the daemon's scrollback grows
	// past this, the new rows (lastScrollbackLen .. current) get
	// shipped as MsgScrollbackAppend. Atomic so the late DataCh-driven
	// re-publish (which may run after the publish goroutine already
	// advanced it) reads a coherent value; all writes happen on the
	// publish goroutine (advance here, reset on clearPending).
	lastScrollbackLen atomic.Int64

	// Per-sub cache of the last TabState we sent. Suppresses
	// re-sends when nothing changed since the previous tick.
	// stateInit goes true after the first push so an all-empty
	// initial state still emits a frame (the client needs the
	// app-cursor=false baseline even when CWD is "" because the
	// PTY hasn't said pwd yet).
	stateInit     bool
	lastCWD       string
	lastFg        string
	lastAppCursor bool
	lastTitle     string
}

func (c *clientConn) handshake() error {
	t, body, err := c.reader.ReadFrame()
	if err != nil {
		return fmt.Errorf("read hello: %w", err)
	}
	if t != protocol.MsgHello {
		return fmt.Errorf("expected MsgHello, got %v", t)
	}
	var hello protocol.Hello
	if _, err := hello.UnmarshalMsg(body); err != nil {
		return fmt.Errorf("unmarshal hello: %w", err)
	}
	if hello.Version != protocol.ProtocolVersion {
		// Send an Error and bail. Phase 0 doesn't try to negotiate
		// older versions.
		_ = c.writeFrame(protocol.MsgError, &protocol.Error{
			Code:    1,
			Message: fmt.Sprintf("unsupported protocol version %d; daemon speaks %d", hello.Version, protocol.ProtocolVersion),
		})
		return fmt.Errorf("version mismatch (client %d, server %d)", hello.Version, protocol.ProtocolVersion)
	}
	host, _ := os.Hostname()
	// Record this connection's claimed client ID on the daemon so
	// other clients / MCP agents can see who's attached.
	c.clientID = hello.ClientID
	c.daemon.registerClient(c)
	return c.writeFrame(protocol.MsgHelloAck, &protocol.HelloAck{
		ServerVersion: protocol.ProtocolVersion,
		ServerID:      fmt.Sprintf("%s:%d", host, os.Getpid()),
		Hostname:      host,
	})
}

// runReadLoop dispatches client commands until the connection is
// closed by the peer or the daemon stops.
func (c *clientConn) runReadLoop() {
	defer c.stopAllSubs()
	for {
		t, body, err := c.reader.ReadFrame()
		if err != nil {
			if err != io.EOF {
				fmt.Fprintf(os.Stderr, "xerottyd: read error: %v\n", err)
			}
			return
		}
		if err := c.dispatch(t, body); err != nil {
			fmt.Fprintf(os.Stderr, "xerottyd: dispatch %v: %v\n", t, err)
			// continue — a bad command shouldn't kill the connection
		}
	}
}

func (c *clientConn) dispatch(t protocol.MsgType, body []byte) error {
	switch t {
	case protocol.MsgAttach:
		var msg protocol.Attach
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		return c.handleAttach(&msg)
	case protocol.MsgDetach:
		c.stopAllSubs()
		c.session = nil
		c.sessionName.Store("")
		return nil
	case protocol.MsgTabCreate:
		var msg protocol.TabCreate
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		return c.handleTabCreate(&msg)
	case protocol.MsgTabClose:
		var msg protocol.TabClose
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		return c.handleTabClose(&msg)
	case protocol.MsgTabFocus:
		var msg protocol.TabFocus
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		if c.session != nil {
			c.session.SetFocusedTab(msg.ID)
		}
		return nil
	case protocol.MsgWindowCreate:
		var msg protocol.WindowCreate
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		return c.handleWindowCreate(&msg)
	case protocol.MsgWindowClose:
		var msg protocol.WindowClose
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		if c.session != nil {
			c.daemon.CloseWindow(c.session, msg.ID)
		}
		return nil
	case protocol.MsgWindowMoveTab:
		var msg protocol.WindowMoveTab
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		if c.session != nil {
			c.daemon.MoveTab(c.session, msg.TabID, msg.ToWindowID, int(msg.Index))
		}
		return nil
	case protocol.MsgWindowGeometry:
		var msg protocol.WindowGeometry
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		if c.session != nil {
			c.session.SetWindowGeometry(msg.ID, msg.PosX, msg.PosY, msg.Width, msg.Height)
		}
		return nil
	case protocol.MsgWindowFocusTab:
		var msg protocol.WindowFocusTab
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		if c.session != nil {
			c.session.SetWindowFocusedTab(msg.WindowID, msg.TabID)
		}
		return nil
	case protocol.MsgResize:
		var msg protocol.Resize
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		return c.handleResize(&msg)
	case protocol.MsgInputBytes:
		var msg protocol.InputBytes
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		return c.handleInput(msg.ID, msg.Bytes)
	case protocol.MsgInputPaste:
		var msg protocol.InputPaste
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		return c.handlePaste(msg.ID, msg.Bytes)
	case protocol.MsgInputImage:
		var msg protocol.InputImage
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		return c.handleImagePaste(&msg)
	case protocol.MsgInputImageChunk:
		var msg protocol.InputImageChunk
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		return c.handleImageChunk(&msg)
	case protocol.MsgClipboardData:
		var msg protocol.ClipboardData
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		return c.handleClipboardData(&msg)
	case protocol.MsgProposalResolve:
		var msg protocol.ProposalResolve
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		if c.session != nil {
			if msg.Approve {
				_, _, _ = c.session.ApproveProposal(int(msg.Index))
			} else {
				c.session.DropProposal(int(msg.Index))
			}
		}
		return nil
	case protocol.MsgClearScrollback:
		var msg protocol.ClearScrollback
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		if c.session != nil {
			if t := c.session.Tab(msg.ID); t != nil {
				// Clear server-side (emu ring + disk) and
				// broadcast to ALL attached clients so their
				// local scrollback mirrors drop too. Without
				// the broadcast, other clients keep showing
				// stale history AND their subscription cursors
				// (lastScrollbackLen) stay pointed at the old
				// length, so they'd silently skip new rows
				// until daemon scrollback grew past the old
				// length again.
				t.Term.ClearScrollback()
				c.daemon.broadcastScrollbackCleared(t.ID)
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown message type %v", t)
	}
}

func (c *clientConn) handleAttach(msg *protocol.Attach) error {
	name := msg.SessionName
	if name == "" {
		name = "default"
	}
	c.session = c.daemon.session(name)
	c.sessionName.Store(c.session.Name)
	// Mark this connection as accepting subscriptions for the session
	// (guarded by subsMu, the same lock subscribe/stopAllSubs use) so
	// the cross-connection create fan-out can safely target us.
	c.subsMu.Lock()
	c.subSession = c.session
	c.subsMu.Unlock()
	created := false
	if msg.NewIfMissing {
		var err error
		if created, err = c.session.EnsureInitialTab(80, 24, ""); err != nil {
			return c.writeFrame(protocol.MsgError, &protocol.Error{Code: 2, Message: err.Error()})
		}
	}
	// Build the Attached frame from a consistent snapshot so its
	// Revision matches the windows/tabs it carries — the client seeds
	// its topology gate from this, so a mismatched revision would let
	// a same-revision broadcast be ignored and leave the client stale.
	snap := c.session.TopologySnapshot()
	if err := c.writeFrame(protocol.MsgAttached, &protocol.Attached{
		SessionName:  c.session.Name,
		Windows:      snap.Windows,
		Tabs:         snap.Tabs,
		FocusedTabID: snap.FocusedTabID,
		Revision:     snap.Revision,
	}); err != nil {
		return err
	}
	// Subscribe to each existing tab — start a publish goroutine
	// per tab that emits CellFull frames whenever the terminal's
	// data channel signals new output. (subscribe is idempotent and
	// keyed by tab ID; a since-changed set self-heals on the next
	// topology broadcast.)
	for _, t := range c.session.Tabs() {
		c.subscribe(c.session, t)
	}
	// If we just spawned the session's first tab, subscribe every
	// attached client to it (a client that attached to the then-empty
	// session must get its publish loop too, not just us) and tell
	// them all about the new topology.
	if created {
		for _, t := range c.session.Tabs() {
			c.daemon.subscribeSessionClients(c.session, t)
		}
		c.daemon.broadcastTopology(c.session)
	}
	// Push the current propose-mode queue so a freshly-attached
	// GUI shows any already-pending proposals in its gate.
	c.daemon.broadcastProposals()
	return nil
}

func (c *clientConn) handleTabCreate(msg *protocol.TabCreate) error {
	if c.session == nil {
		return fmt.Errorf("TabCreate before Attach")
	}
	cols := int(msg.Cols)
	if cols <= 0 {
		cols = 80
	}
	rows := int(msg.Rows)
	if rows <= 0 {
		rows = 24
	}
	// Route through the daemon funnel: it subscribes EVERY attached
	// client (including us) to the new tab and broadcasts the topology
	// (MsgTopologyChanged), so the tab isn't just acked to the creator.
	t, w, err := c.daemon.CreateTab(c.session, msg.WindowID, cols, rows, msg.Cwd)
	if err != nil {
		return c.writeFrame(protocol.MsgError, &protocol.Error{Code: 3, Message: err.Error()})
	}
	// Unicast the ack to the requester, echoing ReqID so it can
	// correlate (the broadcast above is how OTHER clients learn).
	// Report the ACTUAL grid dims, not the requested ones —
	// Session.NewTab clamps to MaxTabDim, and the client (especially
	// daemonsource) sizes its local emulator from this, so handing
	// back the pre-clamp request would desync the mirror.
	return c.writeFrame(protocol.MsgTabCreated, &protocol.TabCreated{
		Info: protocol.TabInfo{
			ID:    t.ID,
			Title: t.Title(),
			Cols:  uint16(t.Term.Width()),
			Rows:  uint16(t.Term.Height()),
		},
		WindowID: w.ID,
		ReqID:    msg.ReqID,
	})
}

func (c *clientConn) handleWindowCreate(msg *protocol.WindowCreate) error {
	if c.session == nil {
		return fmt.Errorf("WindowCreate before Attach")
	}
	w := c.daemon.CreateWindow(c.session, msg.PosX, msg.PosY, msg.Width, msg.Height)
	return c.writeFrame(protocol.MsgWindowCreated, &protocol.WindowCreated{
		Info: protocol.WindowInfo{
			ID:           w.ID,
			PosX:         w.PosX,
			PosY:         w.PosY,
			Width:        w.Width,
			Height:       w.Height,
			TabIDs:       w.TabIDs,
			FocusedTabID: w.FocusedTabID,
		},
		ReqID: msg.ReqID,
	})
}

func (c *clientConn) handleTabClose(msg *protocol.TabClose) error {
	if c.session == nil {
		return nil
	}
	// The funnel unsubscribes EVERY client (including us) before
	// closing the tab, so we don't unsubscribe here ourselves.
	c.daemon.CloseTab(c.session, msg.ID)
	return nil
}

func (c *clientConn) handleResize(msg *protocol.Resize) error {
	if c.session == nil {
		return nil
	}
	t := c.session.Tab(msg.ID)
	if t == nil {
		return nil
	}
	t.Term.Resize(ClampTabDim(int(msg.Cols)), ClampTabDim(int(msg.Rows)))
	// Fan out to EVERY subscriber so all attached clients re-paint at
	// the new dimensions now (a resize emits no PTY output, and the
	// shared DataCh would wake only one of them).
	c.daemon.WakeTabSubscribers(t.ID)
	return nil
}

func (c *clientConn) handleInput(id uint32, b []byte) error {
	if c.session == nil {
		return nil
	}
	t := c.session.Tab(id)
	if t == nil {
		return nil
	}
	_, err := t.Term.Write(b)
	return err
}

// handlePaste routes the bytes through Terminal.Paste so the daemon
// wraps the payload in CSI 200~ / 201~ when the foreground app has
// enabled DECSET 2004 (bracketed paste). This is why InputPaste is
// a separate message type from InputBytes — only the daemon knows
// whether the tab is in bracketed-paste mode at this moment.
func (c *clientConn) handlePaste(id uint32, b []byte) error {
	if c.session == nil {
		return nil
	}
	t := c.session.Tab(id)
	if t == nil {
		return nil
	}
	t.Term.Paste(string(b))
	return nil
}

// handleImagePaste writes the image bytes to a daemon-side temp file
// and types the path into the tab's PTY as if the user had typed it.
// Solves the "paste a screenshot into remote claude code over SSH"
// pain — the file lives on the daemon's machine where Claude Code
// can read it natively without any base64/OSC52 dance.
//
// The path is typed via Paste (so bracketed-paste wrapping applies
// if the app supports it), surrounded by spaces so it doesn't
// concatenate with prior typed input. We don't quote the path
// because the temp filename is constructed to contain only safe
// characters.
//
// Filename hint from the client (if provided) becomes the prefix —
// useful so the user sees a meaningful name in shell history. We
// always append a 16-char random suffix + extension derived from
// MIME to avoid collisions and give the file a recognizable type.
//
// Temp files persist until /tmp is cleaned by the OS or the user;
// the daemon doesn't try to garbage-collect them (they may still be
// open in editors / referenced by long-lived agent sessions).
func (c *clientConn) handleImagePaste(msg *protocol.InputImage) error {
	return c.writeImageAndType(msg.ID, msg.MIME, msg.Filename, msg.Bytes)
}

// handleImageChunk accumulates one chunk of a chunked image paste.
// On the Final chunk it flushes the assembled bytes through the
// same write-temp-file-and-type-path logic as the single-frame
// path. Out-of-order or oversized streams are dropped defensively.
func (c *clientConn) handleImageChunk(msg *protocol.InputImageChunk) error {
	if c.session == nil {
		return nil
	}
	// Don't buffer bytes for a tab that doesn't exist — otherwise a
	// client could pin memory under arbitrary bogus tab IDs that no
	// Final flush will ever target. Also drops any stale partial.
	if c.session.Tab(msg.ID) == nil {
		delete(c.imgAccum, msg.ID)
		return nil
	}
	if c.imgAccum == nil {
		c.imgAccum = make(map[uint32]*imageAssembly)
	}
	asm := c.imgAccum[msg.ID]
	if msg.Seq == 0 {
		// Cap concurrent assemblies so a client can't pin many
		// parallel 256MiB buffers under distinct tab IDs.
		if asm == nil && len(c.imgAccum) >= maxConcurrentImageAssemblies {
			return c.writeFrame(protocol.MsgError, &protocol.Error{
				Code: 14, Message: "image paste: too many concurrent uploads",
			})
		}
		// First chunk (re)starts the assembly — capture metadata.
		asm = &imageAssembly{mime: msg.MIME, filename: msg.Filename}
		c.imgAccum[msg.ID] = asm
	}
	if asm == nil {
		// Got a non-zero seq with no assembly in progress — the
		// stream is corrupt/reordered. Drop it.
		return nil
	}
	if msg.Seq != asm.nextSeq {
		// Sequence gap — abandon the partial image rather than
		// splice in garbage.
		delete(c.imgAccum, msg.ID)
		return nil
	}
	asm.buf = append(asm.buf, msg.Data...)
	asm.nextSeq++
	// Bound memory: a runaway stream shouldn't OOM the daemon.
	if len(asm.buf) > 256*1024*1024 { // 256 MiB hard ceiling
		delete(c.imgAccum, msg.ID)
		return c.writeFrame(protocol.MsgError, &protocol.Error{
			Code: 13, Message: "image paste: exceeds 256MiB ceiling",
		})
	}
	if !msg.Final {
		return nil
	}
	delete(c.imgAccum, msg.ID)
	return c.writeImageAndType(msg.ID, asm.mime, asm.filename, asm.buf)
}

// writeImageAndType writes image bytes to a daemon-side temp file
// and types the path into the tab's PTY. Shared by the single-
// frame (handleImagePaste) and chunked (handleImageChunk) paths.
//
// The path is typed via Paste (so bracketed-paste wrapping applies
// if the app supports it), surrounded by spaces so it doesn't
// concatenate with prior typed input. We don't quote the path
// because the temp filename is constructed to contain only safe
// characters.
//
// Temp files persist until /tmp is cleaned by the OS or the user;
// the daemon doesn't try to garbage-collect them (they may still be
// open in editors / referenced by long-lived agent sessions).
func (c *clientConn) writeImageAndType(tabID uint32, mime, filename string, data []byte) error {
	if c.session == nil {
		return nil
	}
	t := c.session.Tab(tabID)
	if t == nil {
		return nil
	}
	if len(data) == 0 {
		return nil // empty image — nothing to do
	}
	ext := extForMIME(mime)
	prefix := "xerotty-paste"
	if hint := sanitizeFilename(filename); hint != "" {
		prefix += "-" + hint
	}
	f, err := os.CreateTemp("", prefix+"-*"+ext)
	if err != nil {
		return c.writeFrame(protocol.MsgError, &protocol.Error{
			Code: 10, Message: "image paste: temp file: " + err.Error(),
		})
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return c.writeFrame(protocol.MsgError, &protocol.Error{
			Code: 11, Message: "image paste: write: " + err.Error(),
		})
	}
	if err := f.Close(); err != nil {
		return c.writeFrame(protocol.MsgError, &protocol.Error{
			Code: 12, Message: "image paste: close: " + err.Error(),
		})
	}
	t.Term.Paste(" " + f.Name() + " ")
	return nil
}

// handleClipboardData records the client's clipboard contents on the
// session. Future PTY children reading via OSC 52 get this back.
func (c *clientConn) handleClipboardData(msg *protocol.ClipboardData) error {
	if c.session == nil {
		return nil
	}
	c.session.SetClipboard(msg.Text)
	return nil
}

// extForMIME maps common image MIME types to file extensions. Falls
// back to ".bin" for anything unrecognized — tools can still open
// the file, the extension is just a hint.
func extForMIME(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	case "image/tiff":
		return ".tiff"
	case "image/svg+xml":
		return ".svg"
	default:
		return ".bin"
	}
}

// sanitizeFilename keeps only path-safe ASCII so the temp-file name
// doesn't accidentally introduce shell metacharacters when the PTY
// child reads it back. We use this as a prefix; os.CreateTemp adds
// its own random suffix so collision-safety isn't our problem.
func sanitizeFilename(s string) string {
	if s == "" {
		return ""
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s) && len(out) < 32; i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z',
			ch >= 'A' && ch <= 'Z',
			ch >= '0' && ch <= '9',
			ch == '-' || ch == '_' || ch == '.':
			out = append(out, ch)
		default:
			// Skip anything else — spaces, slashes, quotes, etc.
		}
	}
	return string(out)
}

// initialBackfillRows is how many recent scrollback rows the daemon
// ships at subscribe time so reattach restores history. Trade-off:
//   - Too low: reopened windows feel like they "forgot" recent
//     output.
//   - Too high: the initial dump pegs the wire for a few seconds
//     on large sessions.
// 5000 rows ≈ ~200 viewport-fulls of 25-line screens; covers most
// "what was I just looking at?" cases without flooding.
const initialBackfillRows = 5000

// subscribe spawns the per-tab publish goroutine. Seeds
// lastScrollbackLen so the daemon ships at most initialBackfillRows
// of pre-attach history (chunked by sendNewScrollback's batch cap),
// then streams post-attach growth normally. Older-than-backfill
// history stays on the daemon for future scrollback-request
// protocol additions but isn't proactively shipped.
func (c *clientConn) subscribe(sess *Session, t *Tab) {
	c.subsMu.Lock()
	// Only subscribe if this connection is currently attached to the
	// session the tab belongs to. Checked under subsMu (same lock
	// stopAllSubs uses) so a concurrent detach/disconnect can't let a
	// subscribe land after teardown — which would leak the publish
	// loop, since the cancel that stops it has already been closed.
	if c.subSession != sess {
		c.subsMu.Unlock()
		return
	}
	if c.subs == nil {
		c.subs = make(map[uint32]*tabSub)
	}
	if _, exists := c.subs[t.ID]; exists {
		c.subsMu.Unlock()
		return
	}
	sbLen := t.Term.ScrollbackLen()
	backfillFrom := sbLen - initialBackfillRows
	if backfillFrom < 0 {
		backfillFrom = 0
	}
	sub := &tabSub{
		cancel: make(chan struct{}),
		wake:   make(chan struct{}, 1),
	}
	sub.lastScrollbackLen.Store(int64(backfillFrom))
	c.subs[t.ID] = sub
	c.subsMu.Unlock()

	go c.publishLoop(t, sub)
}

func (c *clientConn) unsubscribe(id uint32) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	if sub, ok := c.subs[id]; ok {
		close(sub.cancel)
		delete(c.subs, id)
	}
}

func (c *clientConn) stopAllSubs() {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	// Clear subSession under the lock so any subscribe racing this
	// teardown (from another connection's create fan-out) sees we're
	// no longer attached and skips, rather than adding a sub whose
	// cancel we've already closed.
	c.subSession = nil
	for id, sub := range c.subs {
		close(sub.cancel)
		delete(c.subs, id)
	}
}

// publishLoop watches a tab's DataCh and emits cell frames whenever
// the terminal produces new output. First frame for each tab is a
// CellFull (so the client gets full state); subsequent frames are
// CellDiff (only changed cells) for bandwidth efficiency.
//
// A grid resize (or first publish) invalidates the cached lastGrid
// and forces a CellFull resync. Cursor updates go out at the same
// cadence as cell updates.
//
// Doesn't run a high-rate cursor-blink timer — UIs handle blink
// locally. The select timeout is just a cancel-responsiveness floor.
func (c *clientConn) publishLoop(t *Tab, sub *tabSub) {
	// Initial full paint + cursor + state push so the client has
	// CWD / foreground proc / app-cursor mode before its first
	// render.
	c.sendCellsAndCursor(t, sub)
	c.sendTabState(t, sub)

	// Slow tick for state. CWD + foreground change rarely (cd, a
	// program starts); DECCKM (sub.lastAppCursor) is push-on-change
	// from the emulator callback already — this tick is the
	// fallback in case we miss an edge.
	stateTick := time.NewTicker(750 * time.Millisecond)
	defer stateTick.Stop()

	// exitedCh fires once when the child reaps; we then nil it so the
	// select stops re-firing but the loop STAYS ALIVE. A held/exited
	// tab (user keeps it open to read the final output) still has a
	// live subscription, so a scrollback clear from another client —
	// delivered via sub.wake → clearPending — still reaches this
	// client. The loop ends only on sub.cancel (tab closed / detach).
	exitedCh := t.Exited
	for {
		select {
		case <-sub.cancel:
			return
		case <-t.Term.DataCh:
			c.sendCellsAndCursor(t, sub)
		case <-sub.wake:
			// Per-sub nudge (resize repaint, scrollback clear). Unlike
			// DataCh this reaches THIS subscriber specifically, so
			// every attached client reacts, not just whichever one
			// happened to win the shared channel.
			c.sendCellsAndCursor(t, sub)
		case <-stateTick.C:
			c.sendTabState(t, sub)
		case <-exitedCh:
			// Child reaped. Flush any final cell state first (the
			// shell's "logout"/"exit" line is interesting!), then ship
			// MsgChildExit. Don't return — keep serving the held tab
			// (see exitedCh comment above).
			c.sendCellsAndCursor(t, sub)
			_ = c.writeFrame(protocol.MsgChildExit, &protocol.ChildExit{
				ID:       t.ID,
				ExitCode: atomic.LoadInt32(&t.ExitCode),
			})
			exitedCh = nil
			// Stop polling tab state: cwd/fg-proc can't change on a
			// dead child, and sendTabState reads the PTY fd
			// (ForegroundProcessName), which would race the ptmx being
			// closed once the tab is finally torn down. The loop lives
			// on for sub.cancel + wake (scrollback clear).
			stateTick.Stop()
		case <-time.After(500 * time.Millisecond):
			// Keeps us responsive to cancel. No-op otherwise.
		}
	}
}

// sendTabState pushes the current per-tab metadata (cwd, fg proc,
// app cursor) IF anything changed since last push. Comparing
// against the per-sub cache avoids spamming the wire when nothing
// moved.
func (c *clientConn) sendTabState(t *Tab, sub *tabSub) {
	cwd := t.Term.GetCWD()
	fg := t.Term.ForegroundProcessName()
	appCursor := t.Term.AppCursorMode()
	title := t.Title()
	if sub.stateInit && cwd == sub.lastCWD && fg == sub.lastFg && appCursor == sub.lastAppCursor && title == sub.lastTitle {
		return
	}
	sub.lastCWD = cwd
	sub.lastFg = fg
	sub.lastAppCursor = appCursor
	sub.lastTitle = title
	sub.stateInit = true
	_ = c.writeFrame(protocol.MsgTabState, &protocol.TabState{
		ID:                    t.ID,
		CWD:                   cwd,
		ForegroundProcessName: fg,
		AppCursorMode:         appCursor,
		Title:                 title,
	})
}

// sendCellsAndCursor publishes whatever changed since the last
// publish for this (client, tab) pair: either a CellFull (resync /
// first frame) or a CellDiff. Then a Cursor frame.
func (c *clientConn) sendCellsAndCursor(t *Tab, sub *tabSub) {
	// Atomic snapshot under publishMu so we don't smear cells
	// across a mid-write scroll (the `1, 6573, 3, 4, 5` bug).
	uvGrid := t.Term.SnapshotViewport()
	rows := uint16(len(uvGrid))
	var cols uint16
	if rows > 0 {
		cols = uint16(len(uvGrid[0]))
	}
	grid := make([][]protocol.Cell, rows)
	for r := uint16(0); r < rows; r++ {
		row := make([]protocol.Cell, cols)
		for col := uint16(0); col < cols; col++ {
			cell := uvGrid[r][col]
			row[col] = cellFromUV(&cell)
		}
		grid[r] = row
	}

	// Decide: resync with CellFull (first paint, or dimensions
	// changed), or diff against lastGrid.
	if sub.lastGrid == nil || sub.lastCols != cols || sub.lastRows != rows {
		_ = c.writeFrame(protocol.MsgCellFull, &protocol.CellFull{
			ID:   t.ID,
			Cols: cols,
			Rows: rows,
			Grid: grid,
		})
		sub.lastGrid = grid
		sub.lastCols = cols
		sub.lastRows = rows
	} else {
		diff := computeCellDiff(t.ID, sub.lastGrid, grid)
		if len(diff.Cells) > 0 {
			_ = c.writeFrame(protocol.MsgCellDiff, diff)
			sub.lastGrid = grid
		}
		// If nothing changed (rare — usually only happens when an
		// app emits cursor-only escapes), skip the frame entirely.
	}

	c.sendCursor(t)
	c.sendNewScrollback(t, sub)
}

// scrollbackBatchMax caps how many rows go into a single
// MsgScrollbackAppend frame. A burst like `seq 80000` would
// otherwise produce a multi-megabyte frame that blocks the
// publishLoop for seconds while the client decodes it; chunking
// to 256 rows keeps each frame in the ~20KB range so the GUI
// stays responsive and the rest of the burst streams through
// later publish cycles.
//
// Lower = smoother but more frame overhead. 256 was chosen
// empirically — fits in one or two typical socket-send syscalls
// at 80-col grids.
const scrollbackBatchMax = 256


// sendNewScrollback ships rows that rolled off the top of the
// viewport since the last publish. Reads through
// t.Term.ScrollbackLen / ScrollbackCellAt which transparently
// handle the disk-backed "unlimited" mode — so the wire format
// is mode-agnostic.
//
// Caps each frame at scrollbackBatchMax rows. If more accumulated,
// the rest streams on the next publishLoop tick — DataCh is
// signaled again as the PTY produces more output, or the 500ms
// fallback fires, so the queue drains within a few cycles even
// when the user just walked away from a `seq 80000` burst.
//
// subscribe() seeded sub.lastScrollbackLen with the snapshot at
// attach time, so this naturally streams only post-attach growth.
// Pre-attach back-fill is a separate (future) request/response.
func (c *clientConn) sendNewScrollback(t *Tab, sub *tabSub) {
	// Flush a pending scrollback-clear first, on this (publish)
	// goroutine, so the reset + MsgScrollbackCleared are ordered
	// against the append below rather than racing it from the
	// clearing client's goroutine. The server already dropped its
	// scrollback (ClearScrollback) before flagging, so ScrollbackLen
	// reads 0 here and the append guard short-circuits — no cleared
	// history gets repopulated.
	if sub.clearPending.Swap(false) {
		sub.lastScrollbackLen.Store(0)
		_ = c.writeFrame(protocol.MsgScrollbackCleared, &protocol.ScrollbackCleared{ID: t.ID})
	}
	cur := t.Term.ScrollbackLen()
	last := int(sub.lastScrollbackLen.Load())
	if cur <= last {
		return
	}
	end := cur
	if end-last > scrollbackBatchMax {
		end = last + scrollbackBatchMax
	}
	// Atomic snapshot of the requested range under publishMu so the
	// rows can't shift mid-iteration. (Even in unlimited+disk mode
	// the disk evict-from-memory step happens under publishMu, so
	// here we get a coherent view.)
	uvRows := t.Term.SnapshotScrollbackRange(last, end)
	rows := make([][]protocol.Cell, 0, len(uvRows))
	for _, uvRow := range uvRows {
		row := make([]protocol.Cell, len(uvRow))
		for col := range uvRow {
			cell := uvRow[col]
			row[col] = cellFromUV(&cell)
		}
		rows = append(rows, row)
	}
	_ = c.writeFrame(protocol.MsgScrollbackAppend, &protocol.ScrollbackAppend{
		ID:      t.ID,
		BaseIdx: uint32(last),
		Rows:    rows,
	})
	sub.lastScrollbackLen.Store(int64(end))
	// More to ship? Nudge THIS sub's own wake so the next iteration of
	// its publishLoop continues draining the batch immediately rather
	// than waiting for the 500ms fallback. Using sub.wake (not the
	// shared DataCh) ensures the continuation reaches this subscriber
	// specifically — a DataCh token could be consumed by another
	// client's loop, stalling this one's drain. Non-blocking; cap 1.
	if end < cur {
		select {
		case sub.wake <- struct{}{}:
		default:
		}
	}
}

// computeCellDiff produces a CellDiff containing only cells that
// changed between prev and cur. Both grids must be the same
// dimensions (caller's responsibility — dimension change forces
// CellFull above).
func computeCellDiff(tabID uint32, prev, cur [][]protocol.Cell) *protocol.CellDiff {
	out := &protocol.CellDiff{ID: tabID}
	for r := 0; r < len(cur); r++ {
		for col := 0; col < len(cur[r]); col++ {
			if !cellEqual(prev[r][col], cur[r][col]) {
				out.Cells = append(out.Cells, protocol.CellDiffEntry{
					Row:  uint16(r),
					Col:  uint16(col),
					Cell: cur[r][col],
				})
			}
		}
	}
	return out
}

// cellEqual reports whether two cells are visually identical. The
// generated *_gen.go provides MarshalMsg etc but no Equal — define
// it here to localize the "what counts as a change" decision.
func cellEqual(a, b protocol.Cell) bool {
	return a.Content == b.Content &&
		a.Style == b.Style &&
		a.Width == b.Width &&
		a.FgRGB == b.FgRGB &&
		a.BgRGB == b.BgRGB
}

func (c *clientConn) sendCursor(t *Tab) {
	pos := t.Term.Emu.CursorPosition()
	style, blink, styleSet := t.Term.CursorStyle()
	_ = c.writeFrame(protocol.MsgCursor, &protocol.Cursor{
		ID:       t.ID,
		Row:      uint16(pos.Y),
		Col:      uint16(pos.X),
		Visible:  t.Term.CursorVisible(),
		Style:    style,
		Blink:    blink,
		StyleSet: styleSet,
	})
}

func (c *clientConn) writeFrame(t protocol.MsgType, body protocol.Msg) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return protocol.WriteFrame(c.conn, t, body)
}
