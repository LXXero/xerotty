package daemonsource

import (
	"sync"
	"sync/atomic"

	uv "github.com/charmbracelet/ultraviolet"
	vt "github.com/charmbracelet/x/vt"

	"github.com/LXXero/xerotty/internal/cellconv"
	"github.com/LXXero/xerotty/internal/clientproto"
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
// Scrollback streams over the wire (MsgScrollbackAppend): the
// daemon ships rows as they roll off the top, and this Source
// keeps a bounded local mirror (scrollbackCap) that
// ScrollbackLen/ScrollbackCellAt read from. Pre-attach history is
// partially back-filled; MsgScrollbackCleared drops the mirror.
type Source struct {
	hub   *Hub
	tabID uint32

	mu   sync.Mutex
	emu  *vt.SafeEmulator
	cols int
	rows int

	// onTitle is the GUI's title callback (tabs.go uses this to set
	// tab.Title from OSC 0/2). Daemon ships titles as Title frames;
	// applyTitle fires this if set.
	onTitle func(string)

	// Cached TabState fields. Updated by applyTabState; read by
	// GetCWD / ForegroundProcessName / AppCursorMode.
	cwd       string
	fgName    string
	appCursor atomic.Bool
	altScreen atomic.Bool

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

	// Sliding-window mode (set for "unlimited" scrollback, where a
	// full mirror would be gigabytes — at 112 bytes/cell × ~200 cols
	// a million rows is ~22 GB, the 124 GB-OOM bug). When windowed,
	// `scrollback` holds only a contiguous window [winStart,
	// winStart+len) of the daemon's history; ScrollbackLen reports the
	// daemon's TRUE total (scrollbackTotal) so scroll math + the
	// scrollbar span the whole history, and rows outside the window
	// read as nil until EnsureScrollbackWindow fetches them. Non-
	// windowed (memory mode) keeps the simple full mirror.
	windowed        bool
	winStart        int // absolute index of scrollback[0] (windowed)
	scrollbackTotal int // daemon's full scrollback depth (windowed)
	// reqFrom/reqTo bound the in-flight fetch request — debounces so a
	// fast scroll doesn't spam the daemon. [0,0) means none in flight.
	reqFrom int
	reqTo   int
	// anchor is the absolute index of the viewport center, updated each
	// frame. The window is always trimmed to scrollbackWindowCap rows
	// CENTERED on it, so merges never grow memory past the cap and the
	// rows nearest what you're looking at are the ones kept.
	anchor int

	// Daemon-side search state (windowed mode): the client can't
	// search history it doesn't mirror, so RequestScrollbackSearch
	// ships the query to the daemon and applySearchResults stashes the
	// reply for the GUI to consume via TakeSearchResults. searchReqID
	// correlates them — a late reply for a superseded query is
	// dropped.
	searchReqID    uint64
	searchResults  []protocol.SearchMatch
	searchTrunc    bool
	searchHave     bool
	searchHaveReq  uint64
	// scrollbackLen mirrors len(scrollback) atomically. The renderer
	// historically asked ScrollbackLen once PER CELL (via cellAt) —
	// even now that it's hoisted to once per frame, lock-free reads
	// keep this getter off any future hot path for free. Updated
	// under s.mu wherever scrollback mutates.
	scrollbackLen atomic.Int64

	// lastTitle suppresses redundant SetOnTitle callbacks when the
	// daemon re-ships the same title on each TabState tick.
	lastTitle string

	// onBell is the GUI's bell callback. Fires from applyBell on
	// every MsgBell frame.
	onBell func()

	// cursorVisible / cursorStyle mirror the daemon's reported
	// cursor state (from MsgCursor frames) so the GUI renderer
	// draws the right shape + honors hide. cursorStyle packs the
	// vt enum (bits 0..7) + blink (bit 8) + styleSet (bit 9).
	cursorVisible atomic.Bool
	cursorStyle   atomic.Uint32

	// renderGen counts draw-relevant content changes (applied cell
	// frames, scrollback churn, resize) for the renderer's
	// cell-layer cache. See terminal.Terminal.renderGen.
	renderGen atomic.Uint64

	// Lifecycle.
	dataCh   chan struct{}
	closed   atomic.Bool
	exited   atomic.Bool
	exitCode atomic.Int32
	// vanished is set when the tab disappeared from the daemon's
	// topology (closed by another client / an MCP agent) — distinct
	// from a local child exit. The GUI force-removes vanished tabs
	// regardless of on_child_exit: there's no local child to "hold".
	vanished atomic.Bool
	// vanishedRestart distinguishes the TWO ways a tab can vanish: a
	// remote close on a still-LIVE daemon (false) vs. the daemon process
	// RESTARTING and losing every tab (true — the reattach saw a new
	// InstanceID). The GUI removes the dead tab in both cases, but only a
	// restart-vanish that empties the window triggers a reseat instead of
	// a quit — a remote close emptying the window is a legitimate close.
	// Always written BEFORE vanished so a reader that sees IsVanished()
	// already sees the correct VanishedByRestart().
	vanishedRestart atomic.Bool

	// reconnecting is set while the Hub's connection is down and
	// re-dialing (layer 4b). The shadow emulator keeps its last frame
	// (never cleared), so the GUI renders the frozen viewport — the
	// renderer reads IsReconnecting to dim/badge it. Input is dropped
	// (Write/Paste/Resize) since the remote PTY can't receive it. Cleared
	// once the Hub reconnects and resyncs.
	reconnecting atomic.Bool
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
		windowed:      h.windowed,
	}
	if s.scrollbackCap == 0 {
		s.scrollbackCap = 10000
	}
	s.exitCode.Store(-1)
	s.cursorVisible.Store(true) // visible until daemon says otherwise
	return s
}

// Title returns the last OSC-set title the daemon reported for
// this tab (via TabState). Empty if none yet. Used by the GUI's
// aggregating MCP server's list_tabs.
func (s *Source) Title() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastTitle
}

// TabID returns the daemon-side tab ID this Source is bound to.
// Used by the GUI to ship focus updates (SendTabFocus /
// SendWindowFocusTab) since the local tabs.Tab.ID is the GUI's
// Manager-assigned local index, not the wire-protocol ID.
func (s *Source) TabID() uint32 { return s.tabID }

// HubClient returns the underlying clientproto.Client for sending
// per-tab focus / geometry messages that aren't on the Source's
// own API. Mostly an escape hatch — most callers should use the
// per-tab methods (Write, Paste, Resize, etc.).
func (s *Source) HubClient() *clientproto.Client { return s.hub.client() }

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

// RenderGeneration implements renderer.EmulatorView — see renderGen.
func (s *Source) RenderGeneration() uint64 { return s.renderGen.Load() }

func (s *Source) CellAt(col, row int) *uv.Cell {
	// No s.mu here: s.emu is set once at construction and never
	// reassigned, and SafeEmulator is internally synchronized — the
	// outer lock added nothing but per-cell mutex+defer overhead
	// (perf-visible under animation). Frame coherence is unchanged:
	// per-cell reads could always interleave with router applies.
	return s.emu.CellAt(col, row)
}

// SnapshotViewport returns a consistent copy of the shadow
// emulator's current viewport. Holds s.mu for the duration so
// concurrent applyCellFull/applyCellDiff can't write into the
// cells we're copying out.
func (s *Source) SnapshotViewport() [][]uv.Cell {
	s.mu.Lock()
	defer s.mu.Unlock()
	cols := s.cols
	rows := s.rows
	out := make([][]uv.Cell, rows)
	for r := 0; r < rows; r++ {
		row := make([]uv.Cell, cols)
		for c := 0; c < cols; c++ {
			cell := s.emu.CellAt(c, r)
			if cell != nil {
				row[c] = *cell
			}
		}
		out[r] = row
	}
	return out
}

// SnapshotScrollbackRange returns rows [from, to) from the local
// scrollback mirror as a consistent copy.
func (s *Source) SnapshotScrollbackRange(from, to int) [][]uv.Cell {
	s.mu.Lock()
	defer s.mu.Unlock()
	if from < 0 {
		from = 0
	}
	if to > len(s.scrollback) {
		to = len(s.scrollback)
	}
	if to <= from {
		return nil
	}
	out := make([][]uv.Cell, 0, to-from)
	for r := from; r < to; r++ {
		row := make([]uv.Cell, len(s.scrollback[r]))
		copy(row, s.scrollback[r])
		out = append(out, row)
	}
	return out
}

// SnapshotWindow implements renderer.EmulatorView: it returns a
// consistent rows×cols copy of the visible window at scrollOffset,
// resolving scrollback + grid in ONE s.mu critical section so a
// concurrent applyScrollbackAppend (which appends rows AND drops the
// oldest when over cap — a ring rotation that shifts every index)
// can't tear the grid↔scrollback boundary under the renderer's walk.
// base is the content-row of viewport row 0; gen is the render
// generation observed under the lock. Mirrors terminal.Terminal's
// method.
//
// Indices are ABSOLUTE (0 = oldest), matching ScrollbackLen /
// ScrollbackCellAt: sbLen is the daemon's true depth, and in windowed
// mode the local mirror is offset by winStart, so an absolute index
// outside [winStart, winStart+len) isn't fetched yet and reads as an
// empty cell (EnsureScrollbackWindow pulls it; a later frame fills
// it). s.scrollback / s.emu are read directly (not via the locking
// accessors) to avoid re-entering s.mu.
func (s *Source) SnapshotWindow(scrollOffset, rows, cols int) ([][]uv.Cell, int, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sbLen := int(s.scrollbackLen.Load())
	base := sbLen - scrollOffset
	out := make([][]uv.Cell, rows)
	for row := 0; row < rows; row++ {
		line := make([]uv.Cell, cols)
		contentIdx := base + row
		for col := 0; col < cols; col++ {
			if contentIdx >= 0 && contentIdx < sbLen {
				idx := contentIdx
				if s.windowed {
					idx -= s.winStart
				}
				if idx >= 0 && idx < len(s.scrollback) {
					rowSlice := s.scrollback[idx]
					if col >= 0 && col < len(rowSlice) {
						line[col] = rowSlice[col]
					}
				}
			} else if contentIdx >= sbLen {
				if c := s.emu.CellAt(col, contentIdx-sbLen); c != nil {
					line[col] = *c
				}
			}
		}
		out[row] = line
	}
	return out, base, s.renderGen.Load()
}

// ScrollbackCellAt reads a cell from the client-side scrollback
// mirror at logical row (0 = oldest) and column. Returns nil for
// out-of-range coords; renderer treats nil as empty.
func (s *Source) ScrollbackCellAt(col, row int) *uv.Cell {
	s.mu.Lock()
	defer s.mu.Unlock()
	// `row` is an absolute scrollback index (0 = oldest). In windowed
	// mode the mirror is offset by winStart; outside the window the
	// cell isn't loaded yet (EnsureScrollbackWindow fetches it) and we
	// return nil — the renderer draws an empty cell until it arrives.
	idx := row
	if s.windowed {
		idx = row - s.winStart
	}
	if idx < 0 || idx >= len(s.scrollback) {
		return nil
	}
	rowSlice := s.scrollback[idx]
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
	// Lock-free: see the scrollbackLen field comment. perf showed the
	// locked version + its defer at ~30% of total CPU when the lava
	// animation made every frame walk the grid.
	return int(s.scrollbackLen.Load())
}

func (s *Source) Write(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, nil
	}
	if s.reconnecting.Load() {
		// The daemon connection is down and re-dialing. Drop input
		// rather than erroring or blocking: the remote shell can't
		// receive it anyway, and the io.Writer contract wants a clean
		// "wrote it all" so the caller (input layer) doesn't spam errors
		// or retry. Honest loss — the bytes never reached a live PTY.
		return len(p), nil
	}
	if err := s.hub.client().SendInput(s.tabID, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *Source) Paste(text string) {
	if s.closed.Load() || s.reconnecting.Load() {
		return
	}
	_ = s.hub.client().SendPaste(s.tabID, []byte(text))
}

func (s *Source) Resize(cols, rows int) {
	s.mu.Lock()
	s.cols = cols
	s.rows = rows
	s.emu.Resize(cols, rows)
	s.mu.Unlock()
	if s.reconnecting.Load() {
		// Resize is replayed implicitly on reconnect: the daemon re-sends
		// a CellFull at the live dims, and the GUI re-issues Resize from
		// its current layout. Skip the send to a dead conn.
		return
	}
	_ = s.hub.client().SendResize(s.tabID, uint16(cols), uint16(rows))
}

func (s *Source) Close() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	// Close means "kill the remote tab" (unlike Detach). Tombstone it
	// FIRST so the close survives a dead/dropping connection: if the
	// SendTabClose below is lost, the Hub replays it on reconnect and
	// the snapshot resync won't resurrect the tab. The GUI has already
	// removed the tab (tabs.Manager.CloseTab drops it from the slice
	// immediately), so this never waits on a daemon ack.
	s.hub.addTombstone(s.tabID)
	_ = s.hub.client().SendTabClose(s.tabID)
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

// markVanished is called by the Hub's topology reconcile when the
// daemon reports this tab no longer exists. It flags the source
// closed+exited so the GUI treats it as gone, and unregisters it from
// frame routing. No MsgTabClose is sent — the tab is already gone on
// the daemon. restart distinguishes a daemon-process restart (lost
// every tab) from a remote close on a still-live daemon; see
// vanishedRestart. vanishedRestart is set FIRST so IsVanished()==true
// always implies VanishedByRestart() is already accurate.
func (s *Source) markVanished(restart bool) {
	s.vanishedRestart.Store(restart)
	s.vanished.Store(true)
	s.exited.Store(true)
	s.closed.Store(true)
	s.hub.unregister(s.tabID)
	s.signalDirty()
}

// IsVanished reports whether this tab disappeared from the daemon's
// topology (closed remotely). The GUI uses it to force-remove the tab
// even under on_child_exit=hold — a vanished tab has no child to hold.
func (s *Source) IsVanished() bool { return s.vanished.Load() }

// VanishedByRestart reports whether the vanish was caused by the daemon
// PROCESS restarting (new InstanceID on reattach) rather than a remote
// close on a still-live daemon. The GUI reseats a fresh tab only on a
// restart-vanish; a remote close that empties the window closes it
// normally. Only meaningful once IsVanished() is true.
func (s *Source) VanishedByRestart() bool { return s.vanishedRestart.Load() }

// IsReconnecting reports whether the daemon connection is currently down
// and re-dialing. The renderer uses it to dim/badge the frozen last
// frame so the user sees the tab is stalled, not dead.
func (s *Source) IsReconnecting() bool { return s.reconnecting.Load() }

// setReconnecting flips the reconnecting flag and wakes the render loop
// so the frozen/dimmed frame (and any badge) repaints promptly. Called
// by the Hub's reconnect loop for every Source on connect/disconnect.
func (s *Source) setReconnecting(v bool) {
	if s.reconnecting.Swap(v) != v {
		s.signalDirty()
	}
}

func (s *Source) IsClosed() bool { return s.closed.Load() || s.exited.Load() }

func (s *Source) ChildExitCode() int { return int(s.exitCode.Load()) }

func (s *Source) AppCursorMode() bool { return s.appCursor.Load() }

// IsAltScreen reports the foreground app's alt-screen state, mirrored
// from the daemon via TabState (the client's shadow emulator is fed
// cells, not mode switches, so it can't know this on its own).
func (s *Source) IsAltScreen() bool { return s.altScreen.Load() }

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
	if s.reconnecting.Load() {
		// Frozen: the connection is down. No-op entirely — don't wipe the
		// frozen scrollback the user is still viewing, and don't fire a
		// SendClearScrollback at a dead conn. Scrollback re-syncs on
		// reconnect anyway.
		return
	}
	s.mu.Lock()
	s.scrollback = nil
	s.scrollbackLen.Store(0)
	s.mu.Unlock()
	_ = s.hub.client().SendClearScrollback(s.tabID)
	s.signalDirty()
}

// PasteImage ships the image bytes to the daemon, which writes
// them to a daemon-side temp file and types the path into the
// PTY. That's the whole point of the daemon arc for image paste —
// the file lives on the daemon's machine so Claude Code (or
// whatever's running over SSH on the remote box) can open it
// natively without base64/OSC52.
func (s *Source) PasteImage(mime, filename string, data []byte) error {
	if s.closed.Load() || s.reconnecting.Load() {
		// Frozen / closed: drop the (potentially large, chunked) image
		// paste rather than streaming megabytes into a dead conn's queue.
		return nil
	}
	return s.hub.client().SendImagePaste(s.tabID, mime, filename, data)
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
			// Skip wide-cell placeholders: SetCell on the wide glyph
			// one column left already wrote them, and an explicit
			// write here reads as a partial overwrite of the wide
			// cell — Line.Set blanks the glyph itself.
			if row[c].Width == 0 {
				continue
			}
			cell := uvCellFromProto(row[c])
			s.emu.SetCell(int(c), int(r), &cell)
		}
	}
	s.renderGen.Add(1)
	s.mu.Unlock()
	s.signalDirty()
}

func (s *Source) applyCellDiff(f *protocol.CellDiff) {
	s.mu.Lock()
	for _, e := range f.Cells {
		// Same placeholder skip as applyCellFull — diffs scan
		// row-major, so the wide glyph's own SetCell (one column
		// left, same frame or an earlier one) owns the placeholder.
		if e.Cell.Width == 0 {
			continue
		}
		cell := uvCellFromProto(e.Cell)
		s.emu.SetCell(int(e.Col), int(e.Row), &cell)
	}
	s.renderGen.Add(1)
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
	// Track visibility + style so the GUI renderer reflects the
	// daemon's real cursor state (DECTCEM / DECSCUSR) rather than
	// always drawing the config default. Pack style (vt enum) +
	// blink + styleSet into the atomic.
	s.cursorVisible.Store(f.Visible)
	packed := uint32(f.Style)
	if f.Blink {
		packed |= 1 << 8
	}
	if f.StyleSet {
		packed |= 1 << 9
	}
	s.cursorStyle.Store(packed)
	s.signalDirty()
}

// CursorVisible reports the daemon-reported cursor visibility.
func (s *Source) CursorVisible() bool { return s.cursorVisible.Load() }

// CursorStyle returns the daemon-reported cursor shape (vt enum),
// blink flag, and whether the foreground app explicitly set it.
func (s *Source) CursorStyle() (style uint8, blink bool, styleSet bool) {
	v := s.cursorStyle.Load()
	return uint8(v & 0xFF), (v>>8)&1 != 0, (v>>9)&1 != 0
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
	s.mu.Lock()
	cb := s.onBell
	s.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// SetOnBell registers a callback fired on every MsgBell frame.
// Daemon broadcasts MsgBell when its PTY child emits BEL; this
// is how that signal reaches a GUI tab so it can flash / play a
// sound / increment a bell counter.
func (s *Source) SetOnBell(fn func()) {
	s.mu.Lock()
	s.onBell = fn
	s.mu.Unlock()
}

// SetOnClipboardSet routes to the hub-level clipboard-set handler:
// MsgClipboardSet is session-global (no tab ID), so it's handled
// once per hub rather than per source. Every daemon-backed tab
// installing the same writeLocalClipboard callback is harmless
// (last writer wins, same value).
func (s *Source) SetOnClipboardSet(fn func(string)) {
	s.hub.SetClipboardSetCallback(fn)
}

// SetClipboardProvider is a no-op for daemon sources: OSC 52 GET
// queries are answered server-side from the session clipboard
// (populated by SendClipboardData on copy), so the client doesn't
// provide anything.
func (s *Source) SetClipboardProvider(func() string) {}

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
	s.altScreen.Store(f.AltScreen)
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

// applyScrollbackCleared drops the local scrollback mirror in
// response to a daemon broadcast. Fires for ALL attached clients
// on the tab, not just the one that issued the clear — that's the
// whole point of the broadcast (concurrent viewers stay in sync).
func (s *Source) applyScrollbackCleared(*protocol.ScrollbackCleared) {
	s.mu.Lock()
	s.scrollback = nil
	s.winStart = 0
	s.scrollbackTotal = 0
	s.reqFrom, s.reqTo = 0, 0
	s.scrollbackLen.Store(0)
	s.renderGen.Add(1)
	s.mu.Unlock()
	s.signalDirty()
}

// Windowed-scrollback tunables. Vars, not consts, only so tests can
// shrink them to exercise the fetch path without thousands of rows —
// never mutated in production.
//   - Cap bounds the rows held in memory (~8000 × ~200 cols × 112 B ≈
//     180 MB worst case, typically far less).
//   - Prefetch: when the viewport comes this close to an uncached
//     edge, fetch the adjacent chunk PROACTIVELY (while the current
//     rows stay on screen) so crossing the edge doesn't flash blank.
//   - FetchSpan: rows pulled per request — large for runway, but kept
//     at/under the daemon's per-reply cap (scrollbackRangeMax=4096) so
//     a request is served in ONE contiguous reply. A span larger than
//     that came back clamped and DISJOINT from the window, which made
//     the merge fall back to replace and the viewport fall off the
//     window → a re-request storm that hung the daemon.
var (
	scrollbackWindowCap = 8000
	scrollbackPrefetch  = 1500
	scrollbackFetchSpan = 4000
)

// uvRowsFromProto converts wire rows to cell rows.
func uvRowsFromProto(in [][]protocol.Cell) [][]uv.Cell {
	out := make([][]uv.Cell, len(in))
	for r, row := range in {
		uvRow := make([]uv.Cell, len(row))
		for i, c := range row {
			uvRow[i] = uvCellFromProto(c)
		}
		out[r] = uvRow
	}
	return out
}

// applyScrollbackAppend incorporates rows that rolled off the live
// screen. Non-windowed: append to the full mirror, FIFO-drop past the
// cap. Windowed: track the daemon's true total and extend the cached
// window ONLY when it's anchored at the live end (otherwise the window
// is parked in cold history the user scrolled to — leave it, just
// update the total so the scrollbar still grows).
func (s *Source) applyScrollbackAppend(f *protocol.ScrollbackAppend) {
	s.mu.Lock()
	rows := uvRowsFromProto(f.Rows)

	if !s.windowed {
		s.scrollback = append(s.scrollback, rows...)
		if over := len(s.scrollback) - s.scrollbackCap; over > 0 {
			n := copy(s.scrollback, s.scrollback[over:])
			for i := n; i < len(s.scrollback); i++ {
				s.scrollback[i] = nil
			}
			s.scrollback = s.scrollback[:n]
		}
		s.scrollbackLen.Store(int64(len(s.scrollback)))
		s.renderGen.Add(1)
		s.mu.Unlock()
		s.signalDirty()
		return
	}

	// Windowed: update the authoritative total.
	total := int(f.Total)
	if end := int(f.BaseIdx) + len(rows); end > total {
		total = end
	}
	if total > s.scrollbackTotal {
		s.scrollbackTotal = total
	}
	// Extend the window only if these rows continue it at the high end
	// (fresh window, or contiguous live-tail growth). A parked cold
	// window gets disjoint live appends — ignore the rows there.
	if len(s.scrollback) == 0 {
		s.winStart = int(f.BaseIdx)
		s.scrollback = rows
	} else if int(f.BaseIdx) == s.winStart+len(s.scrollback) {
		s.scrollback = append(s.scrollback, rows...)
	}
	s.evictWindowFrontLocked()
	s.scrollbackLen.Store(int64(s.scrollbackTotal))
	s.renderGen.Add(1)
	s.mu.Unlock()
	s.signalDirty()
}

// applyScrollbackRange installs the rows the daemon returned for a
// ScrollbackRequest. If they're adjacent to (or overlap) the cached
// window it MERGES them so prefetched chunks extend what's on screen
// — no blank flash — then trims back to the cap centered on the
// viewport, so the window never actually grows past scrollbackWindowCap.
// A disjoint range (a big jump) replaces the window.
func (s *Source) applyScrollbackRange(f *protocol.ScrollbackRange) {
	s.mu.Lock()
	if !s.windowed {
		s.mu.Unlock()
		return
	}
	newFrom := int(f.From)
	newRows := uvRowsFromProto(f.Rows)
	newTo := newFrom + len(newRows)
	wLo, wHi := s.winStart, s.winStart+len(s.scrollback)

	if len(s.scrollback) == 0 || newTo < wLo || newFrom > wHi {
		// Disjoint — replace.
		s.winStart = newFrom
		s.scrollback = newRows
	} else {
		// Adjacent/overlapping — merge into the contiguous union, the
		// fetched rows authoritative over any overlap.
		unionLo, unionHi := min2(wLo, newFrom), max2(wHi, newTo)
		merged := make([][]uv.Cell, unionHi-unionLo)
		copy(merged[wLo-unionLo:], s.scrollback)
		copy(merged[newFrom-unionLo:], newRows)
		s.winStart = unionLo
		s.scrollback = merged
	}
	s.reqFrom, s.reqTo = 0, 0 // request satisfied; re-evaluate next frame
	s.trimWindowLocked(s.anchor)
	if end := s.winStart + len(s.scrollback); end > s.scrollbackTotal {
		s.scrollbackTotal = end
		s.scrollbackLen.Store(int64(end))
	}
	s.renderGen.Add(1)
	s.mu.Unlock()
	s.signalDirty()
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// RequestScrollbackSearch ships a search of the tab's FULL scrollback
// to the daemon (which owns the history this windowed client doesn't
// mirror) and returns the request id to correlate the async reply.
// Returns 0 if there's no live connection.
func (s *Source) RequestScrollbackSearch(query string, caseSensitive, regex, wholeWord bool) uint64 {
	s.mu.Lock()
	s.searchReqID++
	id := s.searchReqID
	tabID := s.tabID
	s.mu.Unlock()
	var cli *clientproto.Client
	if s.hub != nil {
		cli = s.hub.client()
	}
	if cli == nil {
		return 0
	}
	cli.SendSearchRequest(tabID, id, query, caseSensitive, regex, wholeWord)
	return id
}

// applySearchResults stashes a daemon search reply if it answers the
// latest request (a stale reply for a superseded query is ignored).
func (s *Source) applySearchResults(f *protocol.SearchResults) {
	s.mu.Lock()
	if f.ReqID != s.searchReqID {
		s.mu.Unlock()
		return
	}
	s.searchResults = f.Matches
	s.searchTrunc = f.Truncated
	s.searchHave = true
	s.searchHaveReq = f.ReqID
	s.mu.Unlock()
	s.signalDirty() // wake the GUI to consume the results
}

// TakeSearchResults returns the latest unconsumed daemon search reply
// once. ok is false when nothing new has arrived since the last take.
// Matches carry ABSOLUTE scrollback line indices (see the daemon's
// runScrollbackSearch); the caller converts to its display space.
func (s *Source) TakeSearchResults() (reqID uint64, matches []protocol.SearchMatch, truncated, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.searchHave {
		return 0, nil, false, false
	}
	s.searchHave = false
	return s.searchHaveReq, s.searchResults, s.searchTrunc, true
}

// evictWindowFrontLocked trims the window to scrollbackWindowCap by
// dropping oldest rows (the live-anchored append path, where the
// newest rows are the ones to keep). Compacts in place so dropped
// rows free promptly. Caller holds s.mu.
func (s *Source) evictWindowFrontLocked() {
	over := len(s.scrollback) - scrollbackWindowCap
	if over <= 0 {
		return
	}
	n := copy(s.scrollback, s.scrollback[over:])
	for i := n; i < len(s.scrollback); i++ {
		s.scrollback[i] = nil
	}
	s.scrollback = s.scrollback[:n]
	s.winStart += over
}

// trimWindowLocked enforces the cap by keeping the scrollbackWindowCap
// rows CENTERED on anchorCenter — i.e. the rows nearest what the user
// is looking at — and dropping the rest from whichever end(s) are
// farther away. This is what keeps "merge" from being a leak: however
// many rows a fetch splices in, the window is immediately bounded back
// to the cap. Compacts in place so dropped rows free promptly. Caller
// holds s.mu.
func (s *Source) trimWindowLocked(anchorCenter int) {
	if len(s.scrollback) <= scrollbackWindowCap {
		return
	}
	wLo := s.winStart
	wHi := s.winStart + len(s.scrollback)
	keepLo := anchorCenter - scrollbackWindowCap/2
	if keepLo < wLo {
		keepLo = wLo
	}
	keepHi := keepLo + scrollbackWindowCap
	if keepHi > wHi {
		keepHi = wHi
		keepLo = keepHi - scrollbackWindowCap
		if keepLo < wLo {
			keepLo = wLo
		}
	}
	if dropBack := wHi - keepHi; dropBack > 0 {
		end := len(s.scrollback) - dropBack
		for i := end; i < len(s.scrollback); i++ {
			s.scrollback[i] = nil
		}
		s.scrollback = s.scrollback[:end]
	}
	if dropFront := keepLo - wLo; dropFront > 0 {
		n := copy(s.scrollback, s.scrollback[dropFront:])
		for i := n; i < len(s.scrollback); i++ {
			s.scrollback[i] = nil
		}
		s.scrollback = s.scrollback[:n]
		s.winStart += dropFront
	}
}

// EnsureScrollbackWindow keeps the cached window covering the visible
// absolute range [from, to), and — the smoothness fix — PREFETCHES the
// adjacent chunk when the viewport comes within scrollbackPrefetch of
// an uncached edge, so a fast scroll crosses the edge into already-
// loaded rows instead of flashing blank. Called once per frame with
// the visible scrollback span. No-op when not windowed.
func (s *Source) EnsureScrollbackWindow(from, to int) {
	if !s.windowed || to <= from {
		return
	}
	s.mu.Lock()
	total := s.scrollbackTotal
	if from < 0 {
		from = 0
	}
	if to > total {
		to = total
	}
	if to <= from {
		s.mu.Unlock()
		return
	}
	s.anchor = (from + to) / 2
	wLo, wHi := s.winStart, s.winStart+len(s.scrollback)

	cached := from >= wLo && to <= wHi
	moreBelow := wLo > 0           // older history exists below the window
	moreAbove := wHi < total       // newer history exists above the window
	nearLow := from < wLo+scrollbackPrefetch
	nearHigh := to > wHi-scrollbackPrefetch
	bigJump := len(s.scrollback) == 0 || to <= wLo || from >= wHi

	if cached && !(moreBelow && nearLow) && !(moreAbove && nearHigh) {
		// Comfortably inside the window, not near an uncached edge.
		s.mu.Unlock()
		return
	}

	// Pick the range to fetch: a big jump centers a span on the target;
	// otherwise extend the window in the direction we're approaching so
	// the fetched chunk is adjacent and merges.
	var lo, hi int
	switch {
	case bigJump:
		mid := (from + to) / 2
		lo = mid - scrollbackFetchSpan/2
		hi = lo + scrollbackFetchSpan
	case moreBelow && nearLow:
		hi = wLo
		lo = wLo - scrollbackFetchSpan
	default: // moreAbove && nearHigh
		lo = wHi
		hi = wHi + scrollbackFetchSpan
	}
	if lo < 0 {
		lo = 0
	}
	if hi > total {
		hi = total
	}
	if hi <= lo {
		s.mu.Unlock()
		return
	}
	if s.reqTo > s.reqFrom && lo >= s.reqFrom && hi <= s.reqTo {
		// An in-flight request already covers this fetch — don't dup.
		s.mu.Unlock()
		return
	}
	s.reqFrom, s.reqTo = lo, hi
	tabID := s.tabID
	s.mu.Unlock()
	var cli *clientproto.Client
	if s.hub != nil {
		cli = s.hub.client()
	}
	if cli != nil {
		cli.SendScrollbackRequest(tabID, lo, hi-lo)
	}
}

// Wake, if set, is called whenever a Source's visible state changes
// (cells, cursor, title, bell, …). The GUI sets it to platform.PostWake
// so the now event-driven render loop breaks out of its idle wait and
// renders — the daemon-source analogue of terminal.Wake for in-process
// PTYs. nil (headless / tests) makes signalDirty a pure DataChan signal.
// The daemon only sends frames on real activity, so an idle tab never
// wakes the loop.
var Wake func()

// signalDirty wakes whoever's reading DataChan AND nudges the GUI's
// render loop (via Wake) so it repaints. Buffered cap=1 + non-blocking
// select: coalesces bursts so we never block the router on a slow GUI.
func (s *Source) signalDirty() {
	select {
	case s.dataCh <- struct{}{}:
	default:
	}
	if Wake != nil {
		Wake()
	}
}

// --- Helpers ---

// uvCellFromProto converts a wire-format Cell back to the
// ultraviolet.Cell the emulator expects. Delegates to
// internal/cellconv (shared with the daemon's hot-upgrade resume
// path) — see that package for the attr-bit remap and the
// wide-cell-placeholder rules.
func uvCellFromProto(p protocol.Cell) uv.Cell { return cellconv.ToUV(p) }

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
