package daemonsource

import (
	"image/color"
	"sync"
	"sync/atomic"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	vt "github.com/charmbracelet/x/vt"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/protocol"
	"github.com/LXXero/xerotty/internal/terminal"
)

// Compile-time check that *Source satisfies terminal.Source. If you
// add a method to terminal.Source, this is what breaks — fix it by
// adding the method here.
var _ terminal.Source = (*Source)(nil)

// Source is a terminal.Source backed by a daemon connection. Holds
// a local shadow *vt.SafeEmulator that's kept in sync with the
// daemon-side tab via incoming CellFull / CellDiff / Cursor /
// TabState frames. The shadow is what the GUI's renderer +
// selection + search + link-detect code reads from — same shape as
// the in-process PTYSource path.
//
// Scrollback: not yet shipped over the wire. Daemon-backed tabs
// report ScrollbackLen()==0 and ScrollbackCellAt returns nil.
// Phase 4+ adds a scrollback streaming protocol; until then the
// shadow only holds the visible viewport.
type Source struct {
	hub   *Hub
	tabID uint32

	mu      sync.Mutex
	emu     *vt.SafeEmulator
	cols    int
	rows    int

	// onTitle is the GUI's title callback (tabs.go uses this to set
	// tab.Title from OSC 0/2). Daemon ships titles as Title frames;
	// applyTitle fires this if set.
	onTitle func(string)

	// Cached TabState fields. Updated by applyTabState; read by
	// GetCWD / ForegroundProcessName / AppCursorMode.
	cwd      string
	fgName   string
	appCursor atomic.Bool

	// Client-side scrollback ring, fed by MsgScrollbackAppend. Each
	// entry is one row of cells. Oldest at index 0, newest at the
	// tail. ScrollbackLen + ScrollbackCellAt read from this; the
	// GUI's renderer + selection + search all walk it transparently
	// through the Source.ScrollbackCellAt method.
	//
	// Capped at scrollbackCap to bound memory — daemon may have
	// disk-backed unlimited scrollback, but the client only mirrors
	// the recent tail. Older rows are dropped on overflow (FIFO).
	scrollback    [][]uv.Cell
	scrollbackCap int

	// lastTitle suppresses redundant SetOnTitle callbacks when the
	// daemon re-ships the same title on each TabState tick.
	lastTitle string

	// Lifecycle.
	dataCh   chan struct{}
	closed   atomic.Bool
	exited   atomic.Bool
	exitCode atomic.Int32
}

// newSource is invoked by Hub. Don't call directly — call Hub.Adopt
// or Hub.NewTab so the source gets registered in the routing table.
func newSource(h *Hub, tabID uint32, cols, rows int) *Source {
	s := &Source{
		hub:           h,
		tabID:         tabID,
		emu:           vt.NewSafeEmulator(cols, rows),
		cols:          cols,
		rows:          rows,
		dataCh:        make(chan struct{}, 1),
		scrollbackCap: h.scrollbackCap,
	}
	if s.scrollbackCap == 0 {
		s.scrollbackCap = 10000
	}
	s.exitCode.Store(-1)
	return s
}

// --- terminal.Source implementation ---

func (s *Source) Emulator() *vt.SafeEmulator {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emu
}

func (s *Source) Width() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols
}

func (s *Source) Height() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows
}

func (s *Source) CellAt(col, row int) *uv.Cell {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emu.CellAt(col, row)
}

// ScrollbackCellAt reads a cell from the client-side scrollback
// mirror at logical row (0 = oldest) and column. Returns nil for
// out-of-range coords; renderer treats nil as empty.
func (s *Source) ScrollbackCellAt(col, row int) *uv.Cell {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row < 0 || row >= len(s.scrollback) {
		return nil
	}
	rowSlice := s.scrollback[row]
	if col < 0 || col >= len(rowSlice) {
		return nil
	}
	c := rowSlice[col]
	return &c
}

// ScrollbackLen returns how many rows of scrollback the client has
// mirrored. May be less than the daemon's actual scrollback length:
// pre-attach rows aren't back-filled in this phase, and the local
// ring drops oldest rows on overflow.
func (s *Source) ScrollbackLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.scrollback)
}

func (s *Source) Write(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, nil
	}
	if err := s.hub.c.SendInput(s.tabID, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *Source) Paste(text string) {
	if s.closed.Load() {
		return
	}
	_ = s.hub.c.SendPaste(s.tabID, []byte(text))
}

func (s *Source) Resize(cols, rows int) {
	s.mu.Lock()
	s.cols = cols
	s.rows = rows
	s.emu.Resize(cols, rows)
	s.mu.Unlock()
	_ = s.hub.c.SendResize(s.tabID, uint16(cols), uint16(rows))
}

func (s *Source) Close() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	_ = s.hub.c.SendTabClose(s.tabID)
	s.hub.unregister(s.tabID)
	// Wake any DataChan reader so they can notice the close.
	s.signalDirty()
}

// Detach drops this client's hold on the tab without killing the
// daemon-side session. Unlike Close, no MsgTabClose is sent — the
// daemon keeps the PTY running so a future Hub.Adopt (or a
// different client) can pick it back up. This is what GUI
// window-close + app-shutdown call so reopening xerotty finds the
// same tabs waiting.
func (s *Source) Detach() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	s.hub.unregister(s.tabID)
	s.signalDirty()
}

func (s *Source) IsClosed() bool { return s.closed.Load() || s.exited.Load() }

func (s *Source) ChildExitCode() int { return int(s.exitCode.Load()) }

func (s *Source) AppCursorMode() bool { return s.appCursor.Load() }

func (s *Source) GetCWD() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cwd
}

func (s *Source) ForegroundProcessName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fgName
}

func (s *Source) DataChan() <-chan struct{} { return s.dataCh }

func (s *Source) SetOnTitle(fn func(string)) {
	s.mu.Lock()
	s.onTitle = fn
	s.mu.Unlock()
}

// SetScrollbackFromConfig: no-op for daemon-backed tabs. The daemon
// owns its own scrollback config; the client GUI doesn't push
// scrollback settings yet. Future protocol addition.
func (s *Source) SetScrollbackFromConfig(*config.Config) {}

// ClearScrollback drops the local scrollback ring AND tells the
// daemon to clear its scrollback too. Doing both means the GUI
// sees an empty scrollback area immediately (no wait for the
// round-trip + next publish) and a future reattach won't pull
// the same history back via backfill.
func (s *Source) ClearScrollback() {
	s.mu.Lock()
	s.scrollback = nil
	s.mu.Unlock()
	_ = s.hub.c.SendClearScrollback(s.tabID)
	s.signalDirty()
}

// PasteImage ships the image bytes to the daemon, which writes
// them to a daemon-side temp file and types the path into the
// PTY. That's the whole point of the daemon arc for image paste —
// the file lives on the daemon's machine so Claude Code (or
// whatever's running over SSH on the remote box) can open it
// natively without base64/OSC52.
func (s *Source) PasteImage(mime, filename string, data []byte) error {
	if s.closed.Load() {
		return nil
	}
	return s.hub.c.SendImagePaste(s.tabID, mime, filename, data)
}

// --- Frame application (called by Hub.router) ---

func (s *Source) applyCellFull(f *protocol.CellFull) {
	s.mu.Lock()
	if int(f.Cols) != s.cols || int(f.Rows) != s.rows {
		s.cols = int(f.Cols)
		s.rows = int(f.Rows)
		s.emu.Resize(s.cols, s.rows)
	}
	for r := uint16(0); r < f.Rows; r++ {
		if int(r) >= len(f.Grid) {
			break
		}
		row := f.Grid[r]
		for c := uint16(0); c < f.Cols; c++ {
			if int(c) >= len(row) {
				break
			}
			cell := uvCellFromProto(row[c])
			s.emu.SetCell(int(c), int(r), &cell)
		}
	}
	s.mu.Unlock()
	s.signalDirty()
}

func (s *Source) applyCellDiff(f *protocol.CellDiff) {
	s.mu.Lock()
	for _, e := range f.Cells {
		cell := uvCellFromProto(e.Cell)
		s.emu.SetCell(int(e.Col), int(e.Row), &cell)
	}
	s.mu.Unlock()
	s.signalDirty()
}

// applyCursor: vt.SafeEmulator doesn't expose a direct cursor
// setter on the public surface — but the renderer reads cursor
// position via CursorPosition() which we can't override. For now,
// we drive cursor via inline VT escapes (the cleanest path that
// doesn't require patching the vt library). This keeps the shadow's
// idea of cursor position correct.
func (s *Source) applyCursor(f *protocol.Cursor) {
	s.mu.Lock()
	// CSI row;col H is 1-indexed.
	_, _ = s.emu.Write([]byte("\x1b[" + itoa(int(f.Row)+1) + ";" + itoa(int(f.Col)+1) + "H"))
	s.mu.Unlock()
	s.signalDirty()
}

func (s *Source) applyTitle(f *protocol.Title) {
	s.mu.Lock()
	cb := s.onTitle
	s.mu.Unlock()
	if cb != nil {
		cb(f.Title)
	}
}

func (s *Source) applyBell(*protocol.Bell) {
	// TODO: route bell into the GUI's bell handler (sound/visual).
	// For now, silently consume — the GUI's bell wiring isn't
	// abstracted yet.
}

func (s *Source) applyChildExit(f *protocol.ChildExit) {
	s.exitCode.Store(f.ExitCode)
	s.exited.Store(true)
	s.signalDirty()
}

func (s *Source) applyTabState(f *protocol.TabState) {
	s.mu.Lock()
	s.cwd = f.CWD
	s.fgName = f.ForegroundProcessName
	cb := s.onTitle
	lastTitle := s.lastTitle
	s.mu.Unlock()
	s.appCursor.Store(f.AppCursorMode)
	// Title changes fire SetOnTitle callbacks the same way the
	// MsgTitle path does for in-process tabs (tabs.NewTab installs
	// a callback that updates tab.Title). Only fire on actual
	// change to avoid spamming the callback every state tick.
	if f.Title != "" && f.Title != lastTitle && cb != nil {
		cb(f.Title)
	}
	if f.Title != lastTitle {
		s.mu.Lock()
		s.lastTitle = f.Title
		s.mu.Unlock()
	}
}

// applyScrollbackAppend appends new rows to the client-side
// scrollback ring. Drops oldest rows when the ring would overflow
// scrollbackCap so total memory stays bounded — daemon may have a
// disk-backed unlimited buffer, but the client only mirrors the
// recent tail.
func (s *Source) applyScrollbackAppend(f *protocol.ScrollbackAppend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range f.Rows {
		uvRow := make([]uv.Cell, len(row))
		for i, c := range row {
			uvRow[i] = uvCellFromProto(c)
		}
		s.scrollback = append(s.scrollback, uvRow)
	}
	if over := len(s.scrollback) - s.scrollbackCap; over > 0 {
		s.scrollback = s.scrollback[over:]
	}
}

// signalDirty wakes whoever's reading DataChan. Buffered cap=1 +
// non-blocking select: coalesces bursts so we never block the
// router on a slow GUI.
func (s *Source) signalDirty() {
	select {
	case s.dataCh <- struct{}{}:
	default:
	}
}

// --- Helpers ---

// uvCellFromProto converts a wire-format Cell back to the
// ultraviolet.Cell the emulator expects. Reverses
// daemon/cell_convert.go::cellFromUV. The attr byte already lines
// up with uv.Attr* bit positions (both packs in the same order, see
// protocol.PackStyle).
func uvCellFromProto(p protocol.Cell) uv.Cell {
	attrs, ulStyle, fgSet, fgIdx, fgIsRGB, bgSet, bgIdx, bgIsRGB := protocol.UnpackStyle(p.Style)
	style := uv.Style{
		Attrs:     uint8(attrs),
		Underline: uv.Underline(ulStyle),
	}
	if fgSet {
		if fgIsRGB {
			style.Fg = rgbColor(p.FgRGB)
		} else {
			// Includes palette index 0 (ANSI black). The fgSet
			// bit is what distinguishes that from "no color".
			style.Fg = ansi.ExtendedColor(fgIdx)
		}
	}
	if bgSet {
		if bgIsRGB {
			style.Bg = rgbColor(p.BgRGB)
		} else {
			style.Bg = ansi.ExtendedColor(bgIdx)
		}
	}
	c := uv.Cell{
		Content: p.Content,
		Style:   style,
		Width:   int(p.Width),
	}
	if c.Content == "" {
		c.Content = " "
	}
	if c.Width == 0 {
		c.Width = 1
	}
	return c
}

// rgbColor unpacks a packed 0xRRGGBB integer into a stdlib
// color.RGBA the emulator can render. Alpha = 255 (opaque).
func rgbColor(rgb uint32) color.Color {
	return color.RGBA{
		R: uint8(rgb >> 16),
		G: uint8(rgb >> 8),
		B: uint8(rgb),
		A: 0xFF,
	}
}

// itoa is a 0-alloc int-to-decimal-string for small positive ints.
// Used in applyCursor's escape construction (hot path on every
// cursor frame).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	if n < 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
