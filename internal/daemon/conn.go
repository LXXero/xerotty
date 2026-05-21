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

	// Subscriptions: per attached tab, a publish goroutine + the
	// state it needs (cancel channel + last-shipped grid for diff
	// computation).
	subsMu sync.Mutex
	subs   map[uint32]*tabSub
}

type tabSub struct {
	cancel   chan struct{}
	// lastGrid is what we last shipped to this client for this
	// tab. Used to compute CellDiff frames — only cells whose
	// contents/style/width changed since last send go on the wire.
	// nil until the first CellFull has been sent. Sized as
	// rows × cols; resize invalidates and forces a fresh CellFull.
	lastGrid [][]protocol.Cell
	lastCols uint16
	lastRows uint16
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
			c.session.CloseWindow(msg.ID)
		}
		return nil
	case protocol.MsgWindowMoveTab:
		var msg protocol.WindowMoveTab
		if _, err := msg.UnmarshalMsg(body); err != nil {
			return err
		}
		if c.session != nil {
			c.session.MoveTab(msg.TabID, msg.ToWindowID, int(msg.Index))
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
		// Empty session — create default window + initial tab.
		_, _, err := c.session.NewTab(0, 80, 24, "")
		if err != nil {
			return c.writeFrame(protocol.MsgError, &protocol.Error{Code: 2, Message: err.Error()})
		}
		tabs = c.session.Tabs()
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
	windows := c.session.Windows()
	windowInfos := make([]protocol.WindowInfo, len(windows))
	for i, w := range windows {
		windowInfos[i] = protocol.WindowInfo{
			ID:           w.ID,
			PosX:         w.PosX,
			PosY:         w.PosY,
			Width:        w.Width,
			Height:       w.Height,
			TabIDs:       w.TabIDs,
			FocusedTabID: w.FocusedTabID,
		}
	}
	if err := c.writeFrame(protocol.MsgAttached, &protocol.Attached{
		SessionName:  c.session.Name,
		Windows:      windowInfos,
		Tabs:         tabInfos,
		FocusedTabID: c.session.FocusedTab(),
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
	t, w, err := c.session.NewTab(msg.WindowID, cols, rows, msg.Cwd)
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
		WindowID: w.ID,
	}); err != nil {
		return err
	}
	c.subscribe(t)
	return nil
}

func (c *clientConn) handleWindowCreate(msg *protocol.WindowCreate) error {
	if c.session == nil {
		return fmt.Errorf("WindowCreate before Attach")
	}
	w := c.session.NewWindow(msg.PosX, msg.PosY, msg.Width, msg.Height)
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
	})
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
		c.subs = make(map[uint32]*tabSub)
	}
	if _, exists := c.subs[t.ID]; exists {
		c.subsMu.Unlock()
		return
	}
	sub := &tabSub{cancel: make(chan struct{})}
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
	// Initial full paint + cursor.
	c.sendCellsAndCursor(t, sub)

	for {
		select {
		case <-sub.cancel:
			return
		case <-t.Term.DataCh:
			c.sendCellsAndCursor(t, sub)
		case <-time.After(500 * time.Millisecond):
			// Keeps us responsive to cancel. No-op otherwise.
		}
	}
}

// sendCellsAndCursor publishes whatever changed since the last
// publish for this (client, tab) pair: either a CellFull (resync /
// first frame) or a CellDiff. Then a Cursor frame.
func (c *clientConn) sendCellsAndCursor(t *Tab, sub *tabSub) {
	cols := uint16(t.Term.Width())
	rows := uint16(t.Term.Height())

	// Build current grid snapshot.
	grid := make([][]protocol.Cell, rows)
	for r := uint16(0); r < rows; r++ {
		row := make([]protocol.Cell, cols)
		for col := uint16(0); col < cols; col++ {
			row[col] = cellFromUV(t.Term.CellAt(int(col), int(r)))
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
