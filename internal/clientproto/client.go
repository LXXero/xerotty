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
	"net"
	"sync"

	"github.com/LXXero/xerotty/internal/protocol"
)

// Client wraps a single connection to a daemon. Concurrency-safe
// for the write side (all Send* methods serialize through writeMu).
// Callers must consume from the channels returned by On* methods
// or back-pressure the read goroutine.
type Client struct {
	conn   net.Conn
	reader *protocol.FrameReader

	writeMu sync.Mutex

	// Inbound channels. Each is buffered to absorb a small burst
	// before back-pressuring the daemon's send loop.
	cellFull   chan *protocol.CellFull
	cellDiff   chan *protocol.CellDiff
	cursor     chan *protocol.Cursor
	title      chan *protocol.Title
	bell       chan *protocol.Bell
	childExit  chan *protocol.ChildExit
	tabCreated    chan *protocol.TabCreated
	windowCreated chan *protocol.WindowCreated
	attached      chan *protocol.Attached
	tabState      chan *protocol.TabState
	scrollback    chan *protocol.ScrollbackAppend
	sbCleared     chan *protocol.ScrollbackCleared
	clipboardSet  chan *protocol.ClipboardSet
	proposals     chan *protocol.ProposalsChanged
	errCh         chan *protocol.Error
	closed     chan struct{}

	// Closed once Run returns. Hand-rolled because we want
	// "read loop exited cleanly or because of an error" to be
	// observable.
	doneMu sync.Mutex
	done   bool
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
	return &Client{
		conn:       conn,
		reader:     protocol.NewFrameReader(conn),
		cellFull:   make(chan *protocol.CellFull, 16),
		cellDiff:   make(chan *protocol.CellDiff, 64),
		cursor:     make(chan *protocol.Cursor, 64),
		title:      make(chan *protocol.Title, 16),
		bell:       make(chan *protocol.Bell, 16),
		childExit:  make(chan *protocol.ChildExit, 16),
		tabCreated:    make(chan *protocol.TabCreated, 4),
		windowCreated: make(chan *protocol.WindowCreated, 4),
		attached:      make(chan *protocol.Attached, 1),
		tabState:      make(chan *protocol.TabState, 32),
		scrollback:    make(chan *protocol.ScrollbackAppend, 32),
		sbCleared:     make(chan *protocol.ScrollbackCleared, 8),
		clipboardSet:  make(chan *protocol.ClipboardSet, 8),
		proposals:     make(chan *protocol.ProposalsChanged, 8),
		errCh:         make(chan *protocol.Error, 4),
		closed:     make(chan struct{}),
	}
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
		return nil, fmt.Errorf("read HelloAck: %w", err)
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
// default window".
func (c *Client) SendTabCreate(windowID uint32, cols, rows uint16, cwd, command string) error {
	return c.send(protocol.MsgTabCreate, &protocol.TabCreate{
		WindowID: windowID, Cols: cols, Rows: rows, Cwd: cwd, Command: command,
	})
}

// SendWindowCreate registers a new logical UI window in the session.
func (c *Client) SendWindowCreate(posX, posY, width, height int32) error {
	return c.send(protocol.MsgWindowCreate, &protocol.WindowCreate{
		PosX: posX, PosY: posY, Width: width, Height: height,
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
	return c.conn.Close()
}

// Run reads frames until the connection closes or errors. Dispatches
// each frame to its channel. Caller should run this in a goroutine
// after Hello + Attach are done.
func (c *Client) Run() {
	defer func() {
		c.doneMu.Lock()
		c.done = true
		c.doneMu.Unlock()
		close(c.closed)
	}()
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
		if err := c.handle(t, body); err != nil {
			c.doneMu.Lock()
			c.exitErr = err
			c.doneMu.Unlock()
			return
		}
	}
}

func (c *Client) handle(t protocol.MsgType, body []byte) error {
	switch t {
	case protocol.MsgCellFull:
		msg := &protocol.CellFull{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		c.cellFull <- msg
	case protocol.MsgCellDiff:
		msg := &protocol.CellDiff{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		c.cellDiff <- msg
	case protocol.MsgCursor:
		msg := &protocol.Cursor{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		c.cursor <- msg
	case protocol.MsgTitle:
		msg := &protocol.Title{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		c.title <- msg
	case protocol.MsgBell:
		msg := &protocol.Bell{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		c.bell <- msg
	case protocol.MsgChildExit:
		msg := &protocol.ChildExit{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		c.childExit <- msg
	case protocol.MsgTabCreated:
		msg := &protocol.TabCreated{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		c.tabCreated <- msg
	case protocol.MsgWindowCreated:
		msg := &protocol.WindowCreated{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		c.windowCreated <- msg
	case protocol.MsgAttached:
		msg := &protocol.Attached{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		c.attached <- msg
	case protocol.MsgTabState:
		msg := &protocol.TabState{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		// Non-blocking — if no one's draining we drop. State pushes
		// are idempotent (always the current value, not deltas) so
		// losing one is fine; the next one re-syncs.
		select {
		case c.tabState <- msg:
		default:
		}
	case protocol.MsgScrollbackAppend:
		msg := &protocol.ScrollbackAppend{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		// Block here — scrollback rows are NOT idempotent, dropping
		// one creates a permanent gap in the user's history. Better
		// to back-pressure the daemon's send loop than to lose data.
		c.scrollback <- msg
	case protocol.MsgScrollbackCleared:
		msg := &protocol.ScrollbackCleared{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		c.sbCleared <- msg
	case protocol.MsgClipboardSet:
		msg := &protocol.ClipboardSet{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		select {
		case c.clipboardSet <- msg:
		default: // drop if no consumer — clipboard sync is best-effort
		}
	case protocol.MsgProposalsChanged:
		msg := &protocol.ProposalsChanged{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		select {
		case c.proposals <- msg:
		default:
		}
	case protocol.MsgError:
		msg := &protocol.Error{}
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		c.errCh <- msg
	default:
		// Unknown message — skip, log to stderr eventually. For
		// Phase 0 just ignore so the connection stays alive.
	}
	return nil
}

func (c *Client) send(t protocol.MsgType, body protocol.Msg) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return protocol.WriteFrame(c.conn, t, body)
}
