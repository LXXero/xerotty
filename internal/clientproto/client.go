// Package clientproto is the client side of the xerotty wire
// protocol. The UI uses it to talk to a local or remote xerottyd;
// integration tests use it to drive xerottyd headlessly.
//
// Phase 0 surface: Dial(), Hello/Attach handshake, TabCreate,
// InputBytes/Resize/TabFocus/TabClose, callbacks for incoming
// CellFull/Cursor/Title/ChildExit. Cell-diff handling lands in
// Phase 1.
package clientproto

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LXXero/xerotty/internal/protocol"
)

// clientWriteProgressWindow is the idle-progress write timeout for the
// outbound writer goroutine (mirror of the daemon's layer-3 deadline).
// After each SUCCESSFUL partial write the deadline is refreshed; the
// conn is declared dead only if NO bytes move for the whole window — so
// a big-but-flowing write (a large paste) is never killed, only a
// genuinely stalled one. No-op over SSH-stdio (SetWriteDeadline is a
// stub there); the client→daemon heartbeat (layer 4d) covers that
// transport.
const clientWriteProgressWindow = 5 * time.Second

// outQueueCap bounds the per-client ordered send queue. Large enough to
// absorb a normal burst (a paste, a flurry of keystrokes) without
// back-pressuring the UI; a wedged daemon fills it and the writer's
// deadline (or the heartbeat) tears the conn down, unblocking senders.
const outQueueCap = 256

const (
	// clientHeartbeatInterval is how often the client pings the daemon.
	// The daemon also pings us independently (~5s); either side's traffic
	// refreshes our liveness clock, so this is mostly to provoke a pong
	// on an otherwise-idle link.
	clientHeartbeatInterval = 5 * time.Second
	// clientLivenessWindow is how long with NO inbound frame at all (not
	// even the daemon's own ping) before we declare the daemon dead and
	// tear the conn down so the Hub re-dials. ~3 missed heartbeats. This
	// is the ONLY fast detector over SSH-stdio, where write deadlines are
	// no-ops — a dead SSH path delivers nothing, so the window trips.
	clientLivenessWindow = 18 * time.Second

	// maxDispatchBacklog caps the decoded-frame queue between the read
	// loop and dispatchLoop. The read loop must never block (it answers
	// daemon pings), so a stalled consumer backlogs here; at the cap the
	// connection is killed instead — a consumer dead for thousands of
	// frames needs the Hub's redial + resync, not more buffering.
	maxDispatchBacklog = 16384
	// dispatchWarnBacklog is where a single warning is logged — the
	// consumer is stalled but recoverable; the log line is the clue.
	dispatchWarnBacklog = 1024
)

// errDispatchOverflow is Run's exit error when the dispatch backlog
// cap is hit (see maxDispatchBacklog).
var errDispatchOverflow = errors.New("clientproto: dispatch backlog overflow — frame consumer stalled")

// outFrame is one queued outbound frame for the writer goroutine.
type outFrame struct {
	typ  protocol.MsgType
	body protocol.Msg
}

// deadlineWriter wraps the conn so each frame write is bounded by an
// idle-progress deadline (see clientWriteProgressWindow). Only the
// writer goroutine uses it, so it needs no lock. Identical in shape to
// the daemon's deadlineWriter — duplicated rather than shared because
// clientproto must not import the daemon package.
type deadlineWriter struct {
	conn   net.Conn
	window time.Duration
}

func (w *deadlineWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		_ = w.conn.SetWriteDeadline(time.Now().Add(w.window))
		n, err := w.conn.Write(p)
		written += n
		p = p[n:]
		if err != nil {
			// Bytes moved before the deadline → progress, not a stall:
			// refresh the window and keep writing the rest. Only a
			// zero-progress timeout (or any non-timeout error) is fatal.
			if n > 0 && isTimeout(err) {
				continue
			}
			return written, err
		}
	}
	_ = w.conn.SetWriteDeadline(time.Time{}) // clear between frames
	return written, nil
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// inboundReader bumps the Client's lastInbound clock on every read that
// returns bytes — at read granularity, BEFORE the bytes are framed and
// dispatched. So a single large in-flight frame keeps the liveness clock
// fresh (the daemon is clearly alive), and the heartbeat reaper only
// fires when truly nothing arrives. This is the read-side half of the
// fix for the hung-but-socket-open daemon: no bytes + no pong ⇒ reap.
type inboundReader struct {
	r io.Reader
	c *Client
}

func (ir *inboundReader) Read(b []byte) (int, error) {
	n, err := ir.r.Read(b)
	if n > 0 {
		ir.c.lastInbound.Store(time.Now().UnixNano())
	}
	return n, err
}

// Client wraps a single connection to a daemon. Concurrency-safe
// for the write side (all Send* methods enqueue onto the async
// writer's bounded queue). Callers must consume from the channels
// returned by On* methods or back-pressure the read goroutine.
type Client struct {
	// transportStderr, when the transport is a subprocess (ssh),
	// holds the tail of its stderr so handshake failures can quote
	// the real reason instead of a bare EOF. Nil for unix sockets.
	transportStderr interface{ Tail() string }

	conn   net.Conn
	reader *protocol.FrameReader

	// Outbound async writer (Phase 10 layer 4a). ALL frame writes go
	// through the single writeLoop goroutine, so a UI-thread producer
	// (a keystroke, a resize, a paste) NEVER blocks in a synchronous
	// socket Write on a hung daemon — it enqueues and moves on. The
	// writer is the sole owner of conn writes, replacing the old write
	// mutex. send() blocks only while the bounded queue is full OR the
	// conn is torn down; the writer's idle-progress deadline (and the
	// heartbeat) guarantee one of those happens promptly.
	outCh   chan outFrame
	outDone chan struct{}
	dw      *deadlineWriter
	// closeOnce guards the shutdown() teardown (stop the writer + close
	// the conn) so Close, a writer error, the heartbeat reaper, and the
	// read loop can all call it without double-closing.
	closeOnce sync.Once

	// Heartbeat liveness (Phase 10 layer 4d). The heartbeat goroutine
	// (started by Run) arms pingPending each tick; the writer flushes the
	// ping OUT-OF-BAND, ahead of the FIFO backlog (via coalesceCh), so a
	// long outbound queue can't delay the probe. Likewise a Pong reply to
	// the daemon's ping is flushed out-of-band (pongPending) so it isn't
	// stuck behind queued input — which would delay our pong and let the
	// daemon falsely reap us.
	//
	// Liveness reaps when BOTH are stale past the window:
	//   - lastPong: a Pong to OUR ping arrived (set in the read loop).
	//     This is the AUTHORITATIVE client-side signal — proof the daemon
	//     received our ping and replied. (Write-progress is NOT used: the
	//     client writes almost nothing, so a "successful" ping write only
	//     means the kernel buffered our bytes — which a SIGSTOP'd /
	//     hung-but-socket-open daemon does happily forever. Keying on
	//     write-progress let our own pings keep the reaper asleep.)
	//   - lastInbound: ANY bytes received from the daemon (set at
	//     read-granularity in the conn wrapper, not per completed frame).
	//     Guards against false-reaping when a pong is merely delayed
	//     behind a large in-flight inbound frame — if bytes are still
	//     arriving, the daemon is alive, so don't reap even if the pong
	//     itself hasn't landed yet. With a SIGSTOP'd daemon NO bytes
	//     arrive and NO pong returns → both stale → reap within ~window.
	coalesceCh  chan struct{}
	pingPending atomic.Bool
	pingNonce   atomic.Uint64
	pongPending atomic.Bool
	pongNonce   atomic.Uint64
	lastInbound atomic.Int64
	lastPong    atomic.Int64
	// heartbeatInterval / livenessWindow default to the package consts;
	// SetHeartbeat overrides them (tests shorten them). Set before Run
	// starts the heartbeat goroutine, so no synchronization is needed.
	heartbeatInterval time.Duration
	livenessWindow    time.Duration

	// Dispatch spill queue between the read loop and dispatchLoop —
	// decoded frames waiting for channel delivery. See Run/dispatchLoop.
	dispMu     sync.Mutex
	dispCond   *sync.Cond
	dispQ      []func() error
	dispClosed bool

	// Inbound channels. Each is buffered to absorb a small burst
	// before back-pressuring the daemon's send loop.
	cellFull      chan *protocol.CellFull
	cellDiff      chan *protocol.CellDiff
	cursor        chan *protocol.Cursor
	title         chan *protocol.Title
	bell          chan *protocol.Bell
	childExit     chan *protocol.ChildExit
	tabCreated    chan *protocol.TabCreated
	windowCreated chan *protocol.WindowCreated
	attached      chan *protocol.Attached
	tabState      chan *protocol.TabState
	scrollback    chan *protocol.ScrollbackAppend
	scrollbackRng chan *protocol.ScrollbackRange
	searchResults chan *protocol.SearchResults
	sbCleared     chan *protocol.ScrollbackCleared
	clipboardSet  chan *protocol.ClipboardSet
	proposals     chan *protocol.ProposalsChanged
	topology      chan *protocol.TopologyChanged
	errCh         chan *protocol.Error
	closed        chan struct{}

	// Closed once Run returns. Hand-rolled because we want
	// "read loop exited cleanly or because of an error" to be
	// observable.
	doneMu  sync.Mutex
	done    bool
	exitErr error
}

// Dial connects to a unix socket at addr.
func Dial(addr string) (*Client, error) {
	conn, err := net.Dial("unix", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return wrap(conn), nil
}

// Wrap takes an arbitrary net.Conn. Used by the SSH-stdio transport
// (Phase 2) and by tests that want to feed an in-process pipe.
func Wrap(conn net.Conn) *Client {
	return wrap(conn)
}

func wrap(conn net.Conn) *Client {
	c := &Client{
		conn:              conn,
		outCh:             make(chan outFrame, outQueueCap),
		outDone:           make(chan struct{}),
		coalesceCh:        make(chan struct{}, 1),
		dw:                &deadlineWriter{conn: conn, window: clientWriteProgressWindow},
		heartbeatInterval: clientHeartbeatInterval,
		livenessWindow:    clientLivenessWindow,
		cellFull:          make(chan *protocol.CellFull, 16),
		cellDiff:          make(chan *protocol.CellDiff, 64),
		cursor:            make(chan *protocol.Cursor, 64),
		title:             make(chan *protocol.Title, 16),
		bell:              make(chan *protocol.Bell, 16),
		childExit:         make(chan *protocol.ChildExit, 16),
		tabCreated:        make(chan *protocol.TabCreated, 4),
		windowCreated:     make(chan *protocol.WindowCreated, 4),
		attached:          make(chan *protocol.Attached, 1),
		tabState:          make(chan *protocol.TabState, 32),
		scrollback:        make(chan *protocol.ScrollbackAppend, 32),
		scrollbackRng:     make(chan *protocol.ScrollbackRange, 8),
		searchResults:     make(chan *protocol.SearchResults, 4),
		sbCleared:         make(chan *protocol.ScrollbackCleared, 8),
		clipboardSet:      make(chan *protocol.ClipboardSet, 8),
		proposals:         make(chan *protocol.ProposalsChanged, 8),
		topology:          make(chan *protocol.TopologyChanged, 8),
		errCh:             make(chan *protocol.Error, 4),
		closed:            make(chan struct{}),
	}
	// Wrap the reader so EVERY successful read bumps lastInbound at byte
	// granularity (not just on completed frames) — a single long inbound
	// frame still counts as the daemon being alive. Done after c exists
	// so the wrapper can reference c.
	c.reader = protocol.NewFrameReader(&inboundReader{r: conn, c: c})
	c.dispCond = sync.NewCond(&c.dispMu)
	// Start the writer before any Send* (Hello sends, then reads the
	// HelloAck synchronously off the reader — the writer must already be
	// draining outCh for that send to flush). Seed both liveness clocks
	// to "now" so the window doesn't trip before the first round-trip.
	now := time.Now().UnixNano()
	c.lastInbound.Store(now)
	c.lastPong.Store(now)
	go c.writeLoop()
	return c
}

// writeLoop is the sole writer of the connection. Each pass it flushes
// the out-of-band PRIORITY frames (a heartbeat ping and/or a pong reply)
// AHEAD of the FIFO backlog, then drains one queued frame — so a ping or
// pong can jump a large outbound queue and isn't delayed by it (a
// delayed pong would let the daemon falsely reap us). Each frame goes
// through the idle-progress deadline; any write error tears the conn
// down (which unblocks the read loop and any producer on a full queue).
func (c *Client) writeLoop() {
	for {
		if err := c.flushPriority(); err != nil {
			c.shutdown()
			return
		}
		select {
		case <-c.outDone:
			return
		case f := <-c.outCh:
			if err := c.writeFrameRaw(f.typ, f.body); err != nil {
				c.shutdown()
				return
			}
		case <-c.coalesceCh:
			// A priority frame was armed — loop back to flushPriority.
		}
	}
}

// flushPriority writes the armed out-of-band ping + pong ahead of the
// FIFO backlog. Pong echoes the most recent ping nonce we received
// (coalesced — the daemon's liveness check doesn't match nonces, it just
// needs a recent pong).
func (c *Client) flushPriority() error {
	if c.pingPending.Swap(false) {
		if err := c.writeFrameRaw(protocol.MsgPing, &protocol.Ping{Nonce: c.pingNonce.Add(1)}); err != nil {
			return err
		}
	}
	if c.pongPending.Swap(false) {
		if err := c.writeFrameRaw(protocol.MsgPong, &protocol.Pong{Nonce: c.pongNonce.Load()}); err != nil {
			return err
		}
	}
	return nil
}

// writeFrameRaw writes one frame through the idle-progress deadline.
// Only the writeLoop goroutine calls it. (No write-progress bookkeeping:
// the client reaper keys on inbound/pong, not outbound — see the
// heartbeat field comment for why outbound progress is meaningless here.)
func (c *Client) writeFrameRaw(t protocol.MsgType, body protocol.Msg) error {
	return protocol.WriteFrame(c.dw, t, body)
}

// heartbeatLoop pings the daemon periodically and reaps the connection
// when it's both UNANSWERED (no pong for a window) AND SILENT (no inbound
// bytes for a window). Pong is the authoritative liveness signal; the
// inbound guard prevents a false reap when a pong is merely delayed
// behind a large in-flight frame (bytes still arriving ⇒ daemon alive).
//
// This is the ONLY fast detector over SSH-stdio (write deadlines are
// no-ops there) AND the only thing that catches a hung-but-socket-open
// daemon (SIGSTOP'd / deadlocked): the kernel keeps accepting our tiny
// pings into its send buffer forever, so outbound progress proves
// nothing — only the absence of any reply + any data does. Started by
// Run (AFTER the Hello handshake, so a ping never jumps ahead of Hello).
// Exits when the conn is torn down.
func (c *Client) heartbeatLoop() {
	ticker := time.NewTicker(c.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.outDone:
			return
		case <-ticker.C:
			pongIdle := time.Since(time.Unix(0, c.lastPong.Load()))
			inboundIdle := time.Since(time.Unix(0, c.lastInbound.Load()))
			if pongIdle > c.livenessWindow && inboundIdle > c.livenessWindow {
				c.shutdown()
				return
			}
			c.requestPing()
		}
	}
}

// SetHeartbeat overrides the ping interval + liveness window. Call
// before Run (which starts the heartbeat). Mainly for tests; the
// defaults (5s / 18s) are right for production.
func (c *Client) SetHeartbeat(interval, window time.Duration) {
	c.heartbeatInterval = interval
	c.livenessWindow = window
}

// requestPing arms the out-of-band ping slot and nudges the writer.
func (c *Client) requestPing() {
	c.pingPending.Store(true)
	select {
	case c.coalesceCh <- struct{}{}:
	default:
	}
}

// requestPong arms the out-of-band pong slot (echoing the daemon's ping
// nonce) and nudges the writer so the reply jumps the FIFO backlog.
func (c *Client) requestPong(nonce uint64) {
	c.pongNonce.Store(nonce)
	c.pongPending.Store(true)
	select {
	case c.coalesceCh <- struct{}{}:
	default:
	}
}

// shutdown stops the async writer and closes the connection. Idempotent
// — called from Close, a writer error, the read loop's exit, and (layer
// 4d) the heartbeat reaper. Closing outDone releases any producer
// blocked on a full queue; closing the conn unblocks the read loop.
func (c *Client) shutdown() {
	c.closeOnce.Do(func() {
		close(c.outDone)
		_ = c.conn.Close()
	})
}

// Hello performs the protocol handshake. Must be called before any
// other Send*. Blocks until the server's HelloAck arrives.
func (c *Client) Hello(clientID string) (*protocol.HelloAck, error) {
	if err := c.send(protocol.MsgHello, &protocol.Hello{
		Version:  protocol.ProtocolVersion,
		ClientID: clientID,
	}); err != nil {
		return nil, err
	}
	t, body, err := c.reader.ReadFrame()
	if err != nil {
		// A bare EOF here is almost always a version-refused
		// handshake against a daemon too old to send MsgError (or
		// whose refusal raced the close). "read HelloAck: EOF"
		// taught users nothing — say what to actually do.
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			// The transport's own stderr usually knows why (ssh auth
			// failures, host key trouble). Only when it's silent does
			// a version-mismatch hint apply.
			if c.transportStderr != nil {
				if tail := c.transportStderr.Tail(); tail != "" {
					return nil, fmt.Errorf("connection died during handshake: %s", tail)
				}
			}
			return nil, fmt.Errorf("server closed during handshake — likely a protocol version mismatch (this client speaks v%d); update xerotty on the host, then hot-swap its daemon with `xerotty serve --upgrade` (sessions survive)", protocol.ProtocolVersion)
		}
		return nil, fmt.Errorf("read HelloAck: %w", err)
	}
	if t == protocol.MsgError {
		// The daemon refused us and said why (e.g. version mismatch).
		// Surface its message verbatim instead of a type-confusion
		// error — plus the fix, since a stale REMOTE daemon is the
		// common cause: a freshly-built binary on disk does not
		// replace the daemon process already holding the socket.
		var e protocol.Error
		if _, err := e.UnmarshalMsg(body); err == nil && e.Message != "" {
			return nil, fmt.Errorf("%s — update xerotty on the host, then run `xerotty serve --upgrade` there (sessions survive)", e.Message)
		}
		return nil, fmt.Errorf("server refused handshake")
	}
	if t != protocol.MsgHelloAck {
		return nil, fmt.Errorf("expected HelloAck, got %v", t)
	}
	var ack protocol.HelloAck
	if _, err := ack.UnmarshalMsg(body); err != nil {
		return nil, fmt.Errorf("unmarshal HelloAck: %w", err)
	}
	return &ack, nil
}

// Attach asks the server to attach to a session. Use sessionName
// "" for default. After Attach returns, Run() should be started to
// process incoming frames.
func (c *Client) Attach(sessionName string, newIfMissing bool) error {
	return c.send(protocol.MsgAttach, &protocol.Attach{
		SessionName:  sessionName,
		NewIfMissing: newIfMissing,
	})
}

// Detach gracefully closes the attachment without killing tabs.
func (c *Client) Detach() error {
	return c.send(protocol.MsgDetach, &protocol.Detach{})
}

// SendTabCreate requests a new tab. windowID=0 means "session's
// default window". Uncorrelated (ReqID 0) — for fire-and-forget
// callers that don't need to match the MsgTabCreated reply. Callers
// that adopt the resulting tab should use SendTabCreateReq.
func (c *Client) SendTabCreate(windowID uint32, cols, rows uint16, cwd string, command []string, shell bool) error {
	return c.SendTabCreateReq(windowID, cols, rows, cwd, command, shell, 0)
}

// SendTabCreateReq is SendTabCreate with a request ID the daemon
// echoes in MsgTabCreated, so the caller can correlate the reply to
// this specific request (see protocol.TabCreate.ReqID).
func (c *Client) SendTabCreateReq(windowID uint32, cols, rows uint16, cwd string, command []string, shell bool, reqID uint64) error {
	return c.SendNamedTabCreateReq(windowID, cols, rows, cwd, command, shell, "", reqID)
}

// SendNamedTabCreateReq is SendTabCreateReq with an idempotency
// label: a non-empty name reuses the session's live tab under that
// label instead of creating (TabCreated.Reused reports which).
// command/shell carry the optional `-e`/`-x` program override (empty
// command = default shell).
func (c *Client) SendNamedTabCreateReq(windowID uint32, cols, rows uint16, cwd string, command []string, shell bool, name string, reqID uint64) error {
	return c.send(protocol.MsgTabCreate, &protocol.TabCreate{
		WindowID: windowID, Cols: cols, Rows: rows, Cwd: cwd, Command: command, CommandShell: shell, Name: name, ReqID: reqID,
	})
}

// SendWindowCreate registers a new logical UI window in the session.
// Uncorrelated (ReqID 0); callers that adopt the resulting window ID
// should use SendWindowCreateReq.
func (c *Client) SendWindowCreate(posX, posY, width, height int32) error {
	return c.SendWindowCreateReq(posX, posY, width, height, 0)
}

// SendWindowCreateReq is SendWindowCreate with a request ID the daemon
// echoes in MsgWindowCreated, so the caller can correlate the reply
// and drop a late ack from a timed-out request.
func (c *Client) SendWindowCreateReq(posX, posY, width, height int32, reqID uint64) error {
	return c.send(protocol.MsgWindowCreate, &protocol.WindowCreate{
		PosX: posX, PosY: posY, Width: width, Height: height, ReqID: reqID,
	})
}

// SendWindowClose tears down a logical UI window. Tabs reassigned.
func (c *Client) SendWindowClose(id uint32) error {
	return c.send(protocol.MsgWindowClose, &protocol.WindowClose{ID: id})
}

// SendWindowMoveTab moves a tab between windows.
func (c *Client) SendWindowMoveTab(tabID, toWindowID uint32, index int32) error {
	return c.send(protocol.MsgWindowMoveTab, &protocol.WindowMoveTab{
		TabID: tabID, ToWindowID: toWindowID, Index: index,
	})
}

// SendWindowGeometry pushes a UI window's new pos/size to the daemon.
func (c *Client) SendWindowGeometry(id uint32, posX, posY, width, height int32) error {
	return c.send(protocol.MsgWindowGeometry, &protocol.WindowGeometry{
		ID: id, PosX: posX, PosY: posY, Width: width, Height: height,
	})
}

// SendWindowFocusTab reports per-window focus changes.
func (c *Client) SendWindowFocusTab(windowID, tabID uint32) error {
	return c.send(protocol.MsgWindowFocusTab, &protocol.WindowFocusTab{
		WindowID: windowID, TabID: tabID,
	})
}

// WindowCreated returns the channel of WindowCreated confirmations.
func (c *Client) WindowCreated() <-chan *protocol.WindowCreated { return c.windowCreated }

// SendTabClose requests a tab close.
func (c *Client) SendTabClose(id uint32) error {
	return c.send(protocol.MsgTabClose, &protocol.TabClose{ID: id})
}

// SendTabFocus tells the server which tab is focused.
func (c *Client) SendTabFocus(id uint32) error {
	return c.send(protocol.MsgTabFocus, &protocol.TabFocus{ID: id})
}

// SendResize asks the daemon to resize a tab's grid.
func (c *Client) SendResize(id uint32, cols, rows uint16) error {
	return c.send(protocol.MsgResize, &protocol.Resize{
		ID: id, Cols: cols, Rows: rows,
	})
}

// SendInput writes raw bytes to a tab's PTY.
func (c *Client) SendInput(id uint32, b []byte) error {
	return c.send(protocol.MsgInputBytes, &protocol.InputBytes{
		ID: id, Bytes: b,
	})
}

// SendPaste sends a paste payload to a tab.
func (c *Client) SendPaste(id uint32, b []byte) error {
	return c.send(protocol.MsgInputPaste, &protocol.InputPaste{
		ID: id, Bytes: b,
	})
}

// SendImagePaste ships an image blob to a tab. The daemon writes it
// to a temp file on the daemon-side filesystem and types the path
// into the PTY — solves "paste a screenshot into remote Claude Code
// over SSH" without any base64 / OSC sequence brittleness.
//
// MIME (e.g. "image/png") hints the file extension; filename hints
// the temp-file prefix. Both are optional.
//
// Always chunked (MsgInputImageChunk) so big screenshots don't
// need a giant single frame. Small images still go in one chunk
// with Final=true. Chunks are sent in order on this connection;
// the daemon reassembles them.
func (c *Client) SendImagePaste(id uint32, mime, filename string, data []byte) error {
	chunk := protocol.ImageChunkSize
	if len(data) == 0 {
		// Empty image — single final chunk so the daemon still
		// gets MIME/filename and can no-op cleanly.
		return c.send(protocol.MsgInputImageChunk, &protocol.InputImageChunk{
			ID: id, MIME: mime, Filename: filename, Seq: 0, Final: true,
		})
	}
	var seq uint32
	for off := 0; off < len(data); off += chunk {
		end := off + chunk
		if end > len(data) {
			end = len(data)
		}
		msg := &protocol.InputImageChunk{
			ID:    id,
			Seq:   seq,
			Final: end >= len(data),
			Data:  data[off:end],
		}
		if seq == 0 {
			// MIME/filename only on the first chunk.
			msg.MIME = mime
			msg.Filename = filename
		}
		if err := c.send(protocol.MsgInputImageChunk, msg); err != nil {
			return err
		}
		seq++
	}
	return nil
}

// SendClipboardData pushes the client's current clipboard contents
// to the daemon so OSC 52 reads on the daemon side can see them.
func (c *Client) SendClipboardData(text string) error {
	return c.send(protocol.MsgClipboardData, &protocol.ClipboardData{
		Text: text,
	})
}

// CellFull returns the channel of incoming full-grid frames.
func (c *Client) CellFull() <-chan *protocol.CellFull { return c.cellFull }

// CellDiff returns the channel of incoming cell-diff frames
// (Phase 1+ — Phase 0 daemon never sends these).
func (c *Client) CellDiff() <-chan *protocol.CellDiff { return c.cellDiff }

// Cursor returns the channel of incoming cursor updates.
func (c *Client) Cursor() <-chan *protocol.Cursor { return c.cursor }

// Title returns the channel of incoming title updates.
func (c *Client) Title() <-chan *protocol.Title { return c.title }

// Bell returns the channel of incoming bell events.
func (c *Client) Bell() <-chan *protocol.Bell { return c.bell }

// ChildExit returns the channel of incoming child-exit events.
func (c *Client) ChildExit() <-chan *protocol.ChildExit { return c.childExit }

// TabCreated returns the channel of TabCreated confirmations
// (one per SendTabCreate call, in order).
func (c *Client) TabCreated() <-chan *protocol.TabCreated { return c.tabCreated }

// Attached returns the channel of Attached responses (one per
// Attach call).
func (c *Client) Attached() <-chan *protocol.Attached { return c.attached }

// TabState returns the channel of slow-changing per-tab metadata
// pushes (cwd, foreground proc, app cursor mode). Daemon emits at
// attach + on change + on a slow tick.
func (c *Client) TabState() <-chan *protocol.TabState { return c.tabState }

// ScrollbackRange returns the channel of on-demand scrollback-range
// replies (answers to SendScrollbackRequest).
func (c *Client) ScrollbackRange() <-chan *protocol.ScrollbackRange { return c.scrollbackRng }

// SearchResults returns the channel of daemon-side search replies
// (answers to SendSearchRequest).
func (c *Client) SearchResults() <-chan *protocol.SearchResults { return c.searchResults }

// ScrollbackAppend returns the channel of scrollback-row pushes.
// Daemon emits a batch each time the visible viewport rolls up
// (new output pushes lines off the top).
func (c *Client) ScrollbackAppend() <-chan *protocol.ScrollbackAppend { return c.scrollback }

// ScrollbackCleared returns the channel of "scrollback was dropped"
// notifications. Broadcast to every client attached to the tab so
// concurrent viewers all drop their local mirrors in sync.
func (c *Client) ScrollbackCleared() <-chan *protocol.ScrollbackCleared { return c.sbCleared }

// ClipboardSet returns the channel of server→client clipboard
// writes (a PTY app issued OSC 52 set). The client puts Text on
// the local OS clipboard.
func (c *Client) ClipboardSet() <-chan *protocol.ClipboardSet { return c.clipboardSet }

// ProposalsChanged returns the channel of propose-mode queue
// updates. The GUI renders an approval gate from the latest list.
func (c *Client) ProposalsChanged() <-chan *protocol.ProposalsChanged { return c.proposals }

// Topology returns the channel of MsgTopologyChanged snapshots —
// broadcast whenever the session's window/tab structure changes
// (from any client or MCP agent). The Hub consumes these to reconcile
// its adopted Sources.
func (c *Client) Topology() <-chan *protocol.TopologyChanged { return c.topology }

// SendProposalResolve approves (apply) or drops (discard) a
// pending propose-mode proposal by index.
func (c *Client) SendProposalResolve(index uint32, approve bool) error {
	return c.send(protocol.MsgProposalResolve, &protocol.ProposalResolve{
		Index: index, Approve: approve,
	})
}

// SendClearScrollback asks the daemon to drop a tab's scrollback.
func (c *Client) SendClearScrollback(id uint32) error {
	return c.send(protocol.MsgClearScrollback, &protocol.ClearScrollback{ID: id})
}

// SendScrollbackRequest asks the daemon for the absolute scrollback
// row range [from, from+count). The reply arrives on ScrollbackRange.
func (c *Client) SendScrollbackRequest(id uint32, from, count int) error {
	return c.send(protocol.MsgScrollbackRequest, &protocol.ScrollbackRequest{
		ID:    id,
		From:  uint32(from),
		Count: uint32(count),
	})
}

// SendSearchRequest asks the daemon to search a tab's full scrollback.
// reqID correlates the async reply on SearchResults; a stale reply is
// dropped by the caller.
func (c *Client) SendSearchRequest(id uint32, reqID uint64, query string, caseSensitive, regex, wholeWord bool) error {
	return c.send(protocol.MsgSearchRequest, &protocol.SearchRequest{
		ID: id, ReqID: reqID, Query: query,
		CaseSensitive: caseSensitive, Regex: regex, WholeWord: wholeWord,
	})
}

// Errors returns the channel of server-side protocol errors.
func (c *Client) Errors() <-chan *protocol.Error { return c.errCh }

// Closed returns a channel closed when Run exits.
func (c *Client) Closed() <-chan struct{} { return c.closed }

// ExitErr returns the error (if any) that caused Run to exit. Nil
// before Run exits, nil on graceful EOF, non-nil on protocol error.
func (c *Client) ExitErr() error {
	c.doneMu.Lock()
	defer c.doneMu.Unlock()
	return c.exitErr
}

// Close closes the connection. Idempotent.
func (c *Client) Close() error {
	c.shutdown()
	return nil
}

// Run reads frames until the connection closes or errors. Dispatches
// each frame to its channel. Caller should run this in a goroutine
// after Hello + Attach are done.
func (c *Client) Run() {
	defer func() {
		// Stop the writer when the read loop ends (peer closed the conn,
		// or a frame errored) so its goroutine doesn't linger writing
		// into a dead socket. Close the dispatch queue AFTER shutdown:
		// shutdown closes outDone, which aborts any delivery the
		// dispatcher is blocked in, so it can observe the close and exit.
		c.shutdown()
		c.closeDispatch()
		c.doneMu.Lock()
		c.done = true
		c.doneMu.Unlock()
		close(c.closed)
	}()
	// Heartbeat starts here, not in wrap(): Hello has already completed
	// by the time Run is invoked, so an out-of-band ping can't slip
	// ahead of the Hello frame and break the daemon's handshake reader.
	go c.heartbeatLoop()
	// The dispatcher owns all channel delivery so the read loop can
	// NEVER be blocked by a stalled consumer. Before this split, one
	// full payload channel wedged the read loop mid-send — the daemon's
	// next ping was never read, no pong went back, and the daemon
	// reaped a perfectly-alive client (then the silent redial's full
	// resync flashed the screen). Liveness now depends only on the
	// socket being drained, which the read loop guarantees.
	go c.dispatchLoop()
	for {
		t, body, err := c.reader.ReadFrame()
		if err != nil {
			c.doneMu.Lock()
			if !errors.Is(err, net.ErrClosed) {
				c.exitErr = err
			}
			c.doneMu.Unlock()
			return
		}
		// Control frames are handled inline — they must work even when
		// every consumer is stalled; that's their whole point.
		switch t {
		case protocol.MsgPing:
			// The daemon is probing us — reply OUT-OF-BAND (priority
			// slot, not the FIFO outCh) so our pong jumps ahead of any
			// queued input. A pong stuck behind a big paste would delay
			// it past the daemon's window and get us falsely reaped
			// (finding 3).
			msg := &protocol.Ping{}
			if _, err := msg.UnmarshalMsg(body); err != nil {
				c.doneMu.Lock()
				c.exitErr = err
				c.doneMu.Unlock()
				return
			}
			c.requestPong(msg.Nonce)
			continue
		case protocol.MsgPong:
			// Reply to OUR heartbeat ping — the authoritative proof the
			// daemon's read+respond path is alive. Refreshes the pong
			// clock; the reaper needs this OR fresh inbound to stay
			// asleep.
			c.lastPong.Store(time.Now().UnixNano())
			continue
		}
		deliver, err := c.decode(t, body)
		if err != nil {
			c.doneMu.Lock()
			c.exitErr = err
			c.doneMu.Unlock()
			return
		}
		if deliver == nil {
			continue
		}
		if !c.enqueueDispatch(deliver) {
			c.doneMu.Lock()
			c.exitErr = errDispatchOverflow
			c.doneMu.Unlock()
			return
		}
	}
}

// decode unmarshals one payload frame (the body is only valid until
// the next ReadFrame, so decoding happens in the read loop) and
// returns the delivery step for the dispatcher to run. A nil deliver
// with nil error means the frame is consumed (dropped or unknown).
// Ping/Pong never reach here — Run handles them inline.
func (c *Client) decode(t protocol.MsgType, body []byte) (func() error, error) {
	switch t {
	case protocol.MsgCellFull:
		msg := &protocol.CellFull{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return nil, err
		}
		return func() error { return deliverOr(c, c.cellFull, msg) }, nil
	case protocol.MsgCellDiff:
		msg := &protocol.CellDiff{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return nil, err
		}
		return func() error { return deliverOr(c, c.cellDiff, msg) }, nil
	case protocol.MsgCursor:
		msg := &protocol.Cursor{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return nil, err
		}
		return func() error { return deliverOr(c, c.cursor, msg) }, nil
	case protocol.MsgTitle:
		msg := &protocol.Title{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return nil, err
		}
		return func() error { return deliverOr(c, c.title, msg) }, nil
	case protocol.MsgBell:
		msg := &protocol.Bell{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return nil, err
		}
		return func() error { return deliverOr(c, c.bell, msg) }, nil
	case protocol.MsgChildExit:
		msg := &protocol.ChildExit{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return nil, err
		}
		return func() error { return deliverOr(c, c.childExit, msg) }, nil
	case protocol.MsgTabCreated:
		msg := &protocol.TabCreated{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return nil, err
		}
		return func() error { return deliverOr(c, c.tabCreated, msg) }, nil
	case protocol.MsgWindowCreated:
		msg := &protocol.WindowCreated{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return nil, err
		}
		return func() error { return deliverOr(c, c.windowCreated, msg) }, nil
	case protocol.MsgAttached:
		msg := &protocol.Attached{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return nil, err
		}
		return func() error { return deliverOr(c, c.attached, msg) }, nil
	case protocol.MsgTabState:
		msg := &protocol.TabState{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return nil, err
		}
		// Non-blocking — if no one's draining we drop. State pushes
		// are idempotent (always the current value, not deltas) so
		// losing one is fine; the next one re-syncs.
		return func() error {
			select {
			case c.tabState <- msg:
			default:
			}
			return nil
		}, nil
	case protocol.MsgScrollbackAppend:
		msg := &protocol.ScrollbackAppend{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return nil, err
		}
		// Deliver in order, waiting for the consumer — scrollback rows
		// are NOT idempotent, dropping one creates a permanent gap in
		// the user's history. A stalled consumer backlogs the dispatch
		// queue (bounded), not the socket.
		return func() error { return deliverOr(c, c.scrollback, msg) }, nil
	case protocol.MsgScrollbackRange:
		msg := &protocol.ScrollbackRange{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return nil, err
		}
		// Wait, don't drop: a dropped range leaves a permanent hole
		// in the window the client just scrolled to. Buffered + rare.
		return func() error { return deliverOr(c, c.scrollbackRng, msg) }, nil
	case protocol.MsgSearchResults:
		msg := &protocol.SearchResults{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return nil, err
		}
		return func() error { return deliverOr(c, c.searchResults, msg) }, nil
	case protocol.MsgScrollbackCleared:
		msg := &protocol.ScrollbackCleared{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return nil, err
		}
		return func() error { return deliverOr(c, c.sbCleared, msg) }, nil
	case protocol.MsgClipboardSet:
		msg := &protocol.ClipboardSet{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return nil, err
		}
		return func() error {
			select {
			case c.clipboardSet <- msg:
			default: // drop if no consumer — clipboard sync is best-effort
			}
			return nil
		}, nil
	case protocol.MsgProposalsChanged:
		msg := &protocol.ProposalsChanged{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return nil, err
		}
		return func() error {
			select {
			case c.proposals <- msg:
			default:
			}
			return nil
		}, nil
	case protocol.MsgTopologyChanged:
		msg := &protocol.TopologyChanged{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return nil, err
		}
		// Wait for the consumer — topology snapshots are revision-gated
		// and idempotent, but dropping one risks the client missing a
		// structural change until the next mutation.
		return func() error { return deliverOr(c, c.topology, msg) }, nil
	case protocol.MsgError:
		msg := &protocol.Error{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return nil, err
		}
		return func() error { return deliverOr(c, c.errCh, msg) }, nil
	default:
		// Unknown message — skip, log to stderr eventually. For
		// Phase 0 just ignore so the connection stays alive.
		return nil, nil
	}
}

// deliverOr sends msg to ch, aborting when the connection tears down
// so the dispatcher can't leak blocked on a consumer that stopped
// draining (route() returns on Closed and abandons the channels).
func deliverOr[T any](c *Client, ch chan T, msg T) error {
	select {
	case ch <- msg:
		return nil
	case <-c.outDone:
		return net.ErrClosed
	}
}

// dispatchLoop delivers decoded frames to the payload channels in
// arrival order. It is the ONLY goroutine doing channel delivery, so
// FIFO ordering across message types is preserved exactly as the old
// in-read-loop dispatch did it — just decoupled, so a stalled consumer
// backlogs this queue instead of wedging the socket read loop (which
// must stay free to answer daemon pings; see Run).
func (c *Client) dispatchLoop() {
	for {
		c.dispMu.Lock()
		for len(c.dispQ) == 0 && !c.dispClosed {
			c.dispCond.Wait()
		}
		if len(c.dispQ) == 0 {
			c.dispMu.Unlock()
			return
		}
		d := c.dispQ[0]
		c.dispQ[0] = nil
		c.dispQ = c.dispQ[1:]
		if len(c.dispQ) == 0 {
			c.dispQ = nil // release the drained backing array
		}
		c.dispMu.Unlock()
		if err := d(); err != nil {
			// Delivery aborts only on teardown (outDone closed) — make
			// sure the teardown completes and stop delivering.
			c.shutdown()
			return
		}
	}
}

// enqueueDispatch queues one delivery for dispatchLoop. Returns false
// when the backlog cap is hit — the consumer has been stalled long
// enough that killing the connection (and letting the Hub redial with
// a clean resync) beats unbounded buffering.
func (c *Client) enqueueDispatch(d func() error) bool {
	c.dispMu.Lock()
	defer c.dispMu.Unlock()
	if c.dispClosed {
		return true // tearing down; frame is moot
	}
	if len(c.dispQ) >= maxDispatchBacklog {
		return false
	}
	if len(c.dispQ) == dispatchWarnBacklog {
		log.Printf("clientproto: dispatch backlog reached %d frames — consumer stalled?", len(c.dispQ))
	}
	c.dispQ = append(c.dispQ, d)
	c.dispCond.Signal()
	return true
}

// closeDispatch lets dispatchLoop drain what's queued and exit. Called
// from Run's teardown after shutdown() — outDone is already closed, so
// any delivery the dispatcher is blocked in aborts immediately.
func (c *Client) closeDispatch() {
	c.dispMu.Lock()
	c.dispClosed = true
	c.dispMu.Unlock()
	c.dispCond.Broadcast()
}

func (c *Client) send(t protocol.MsgType, body protocol.Msg) error {
	select {
	case c.outCh <- outFrame{typ: t, body: body}:
		return nil
	case <-c.outDone:
		return net.ErrClosed
	}
}
