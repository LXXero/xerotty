package daemon

import (
	"fmt"
	"io"
	"net"
	"os"
	"sync"
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
		daemon: d,
		conn:   conn,
		reader: protocol.NewFrameReader(conn),
	}
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

	session *Session // nil until Attach succeeds

	// Subscriptions: per attached tab, the goroutine pumping
	// PTY-data → CellFull frames. Cancelled when client detaches
	// or disconnects.
	subsMu sync.Mutex
	subs   map[uint32]chan struct{} // tab id → cancel channel
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
	return c.writeFrame(protocol.MsgHelloAck, &protocol.HelloAck{
		ServerVersion: protocol.ProtocolVersion,
		ServerID:      fmt.Sprintf("%s:%d", host, os.Getpid()),
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
			c.session.SetFocused(msg.ID)
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
		// Phase 0: treat paste as raw input. Bracketed-paste
		// wrapping is a Phase 1 enhancement.
		return c.handleInput(msg.ID, msg.Bytes)
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
	tabs := c.session.Tabs()
	if len(tabs) == 0 && msg.NewIfMissing {
		t, err := c.session.NewTab(80, 24, "")
		if err != nil {
			return c.writeFrame(protocol.MsgError, &protocol.Error{Code: 2, Message: err.Error()})
		}
		tabs = []*Tab{t}
	}
	tabInfos := make([]protocol.TabInfo, len(tabs))
	for i, t := range tabs {
		tabInfos[i] = protocol.TabInfo{
			ID:    t.ID,
			Title: t.Title,
			Cols:  uint16(t.Term.Width()),
			Rows:  uint16(t.Term.Height()),
		}
	}
	if err := c.writeFrame(protocol.MsgAttached, &protocol.Attached{
		SessionName: c.session.Name,
		Tabs:        tabInfos,
		FocusedID:   c.session.Focused(),
	}); err != nil {
		return err
	}
	// Subscribe to each existing tab — start a publish goroutine
	// per tab that emits CellFull frames whenever the terminal's
	// data channel signals new output.
	for _, t := range tabs {
		c.subscribe(t)
	}
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
	t, err := c.session.NewTab(cols, rows, msg.Cwd)
	if err != nil {
		return c.writeFrame(protocol.MsgError, &protocol.Error{Code: 3, Message: err.Error()})
	}
	if err := c.writeFrame(protocol.MsgTabCreated, &protocol.TabCreated{
		Info: protocol.TabInfo{
			ID:    t.ID,
			Title: t.Title,
			Cols:  uint16(cols),
			Rows:  uint16(rows),
		},
	}); err != nil {
		return err
	}
	c.subscribe(t)
	return nil
}

func (c *clientConn) handleTabClose(msg *protocol.TabClose) error {
	if c.session == nil {
		return nil
	}
	c.unsubscribe(msg.ID)
	c.session.CloseTab(msg.ID)
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
	t.Term.Resize(int(msg.Cols), int(msg.Rows))
	t.dirty.Add(1)
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

// subscribe spawns the per-tab publish goroutine.
func (c *clientConn) subscribe(t *Tab) {
	c.subsMu.Lock()
	if c.subs == nil {
		c.subs = make(map[uint32]chan struct{})
	}
	if _, exists := c.subs[t.ID]; exists {
		c.subsMu.Unlock()
		return
	}
	cancel := make(chan struct{})
	c.subs[t.ID] = cancel
	c.subsMu.Unlock()

	go c.publishLoop(t, cancel)
}

func (c *clientConn) unsubscribe(id uint32) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	if cancel, ok := c.subs[id]; ok {
		close(cancel)
		delete(c.subs, id)
	}
}

func (c *clientConn) stopAllSubs() {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	for id, cancel := range c.subs {
		close(cancel)
		delete(c.subs, id)
	}
}

// publishLoop watches a tab's DataCh and emits a CellFull frame on
// every wake. Phase 0 deliberately ships full-frame on every change
// because it's simplest; cell-diff comes in Phase 1.
//
// Also sends a Cursor frame at the same cadence so the UI's cursor
// position stays accurate.
//
// Also sends an initial CellFull right away so attaching clients
// see whatever was on screen before they connected.
func (c *clientConn) publishLoop(t *Tab, cancel <-chan struct{}) {
	// initial paint
	c.sendCellFull(t)
	c.sendCursor(t)

	for {
		select {
		case <-cancel:
			return
		case <-t.Term.DataCh:
			c.sendCellFull(t)
			c.sendCursor(t)
		case <-time.After(500 * time.Millisecond):
			// Cursor blink wakes — UI handles blink locally so we
			// don't actually need to send anything. The timeout
			// keeps us responsive to cancel without a tight loop.
		}
	}
}

func (c *clientConn) sendCellFull(t *Tab) {
	cols := t.Term.Width()
	rows := t.Term.Height()
	grid := make([][]protocol.Cell, rows)
	for r := 0; r < rows; r++ {
		row := make([]protocol.Cell, cols)
		for col := 0; col < cols; col++ {
			cell := t.Term.CellAt(col, r)
			row[col] = cellFromUV(cell)
		}
		grid[r] = row
	}
	_ = c.writeFrame(protocol.MsgCellFull, &protocol.CellFull{
		ID:   t.ID,
		Cols: uint16(cols),
		Rows: uint16(rows),
		Grid: grid,
	})
}

func (c *clientConn) sendCursor(t *Tab) {
	pos := t.Term.Emu.CursorPosition()
	_ = c.writeFrame(protocol.MsgCursor, &protocol.Cursor{
		ID:      t.ID,
		Row:     uint16(pos.Y),
		Col:     uint16(pos.X),
		Visible: true,
		Style:   0, // block
	})
}

func (c *clientConn) writeFrame(t protocol.MsgType, body protocol.Msg) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return protocol.WriteFrame(c.conn, t, body)
}
