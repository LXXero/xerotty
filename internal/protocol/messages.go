package protocol

//go:generate msgp -unexported=false

// ProtocolVersion is the wire-format version this build of xerotty
// and xerottyd speak. Bump on any backwards-incompatible change to
// any of the message structs below. Minor field additions are NOT
// breaking — msgpack lets newer servers send fields older clients
// ignore.
// ProtocolVersion bumped to 2 when Style packing grew the fgSet /
// bgSet flag bits to distinguish "no color" from "ANSI black
// (palette idx 0)". Same wire layout otherwise; old clients would
// misinterpret bits 30/31 as "reserved zero" and render black as
// default. Spike branch, no compat shims.
const ProtocolVersion uint16 = 2

// MsgType discriminates frame bodies. The codec writes a single
// MsgType byte right after the length prefix, then the msgpack-
// encoded payload struct.
//
// IDs are stable — append new types, never reuse a removed ID's
// number. Add corresponding struct + Encode/Decode wiring below.
type MsgType uint8

const (
	MsgHello             MsgType = 1  // client → server
	MsgHelloAck          MsgType = 2  // server → client
	MsgAttach            MsgType = 3  // client → server: attach to a session
	MsgAttached          MsgType = 4  // server → client: attach OK, here's tab list
	MsgDetach            MsgType = 5  // client → server: detach gracefully
	MsgError             MsgType = 6  // server → client: protocol-level error

	MsgTabCreate         MsgType = 10 // client → server
	MsgTabClose          MsgType = 11 // client → server
	MsgTabFocus          MsgType = 12 // client → server
	MsgTabCreated        MsgType = 13 // server → client (echo + assigned ID)

	MsgWindowCreate      MsgType = 14 // client → server: new UI window
	MsgWindowClose       MsgType = 15 // client → server: tear down a window
	MsgWindowCreated     MsgType = 16 // server → client (echo + assigned ID)
	MsgWindowMoveTab     MsgType = 17 // client → server: drag tab between windows
	MsgWindowGeometry    MsgType = 18 // client → server: window pos/size moved
	MsgWindowFocusTab    MsgType = 19 // client → server: focused tab within window changed

	MsgResize            MsgType = 20 // client → server (per-tab cols/rows)
	MsgInputBytes        MsgType = 21 // client → server: raw bytes for PTY
	MsgInputPaste        MsgType = 22 // client → server: paste (may be longer)
	MsgInputImage        MsgType = 23 // client → server: paste an image (binary blob)
	MsgClipboardData     MsgType = 24 // either direction: clipboard contents

	MsgCellFull          MsgType = 30 // server → client: full grid (initial / resync)
	MsgCellDiff          MsgType = 31 // server → client: changed cells
	MsgCursor            MsgType = 32 // server → client: cursor position/style
	MsgTitle             MsgType = 33 // server → client: tab title (from OSC)
	MsgBell              MsgType = 34 // server → client
	MsgChildExit         MsgType = 35 // server → client: PTY child exited
	MsgTabState          MsgType = 36 // server → client: cwd, foreground proc, app cursor
	MsgScrollbackAppend  MsgType = 37 // server → client: new scrollback rows
	MsgScrollbackCleared MsgType = 38 // server → client: scrollback was cleared (broadcast to all attached)

	MsgClearScrollback   MsgType = 40 // client → server: drop scrollback (Ctrl+L hard clear path)
)

// Hello is the first frame a client sends after connecting. The
// server validates the version and replies with HelloAck.
type Hello struct {
	Version  uint16 `msg:"version"`
	ClientID string `msg:"client_id"`            // free-form: "xerotty-ui-laptop", "claude-code-1"
	Caps     []string `msg:"caps,omitempty"`     // optional capability strings, reserved for future use
}

// HelloAck answers a Hello. ServerVersion lets the client decide
// whether to proceed (the client may downgrade behavior to match).
//
// Hostname is the daemon's host (typically os.Hostname() — for SSH
// transports it's the remote box's name). UIs use this to render
// per-tab host badges so users know which machine each tab lives
// on when they have a mix of local + remote daemons attached.
type HelloAck struct {
	ServerVersion uint16 `msg:"server_version"`
	ServerID      string `msg:"server_id"`        // unique per daemon instance: hostname:pid
	Hostname      string `msg:"hostname,omitempty"` // just the hostname, for UI display
}

// Attach asks the server to attach this connection to a session. A
// session is a collection of tabs that survives detach. Empty
// SessionName means "default session" (which the daemon creates if
// missing). NewIfMissing creates a fresh tab in the session if the
// session has no tabs yet.
type Attach struct {
	SessionName    string `msg:"session_name"`
	NewIfMissing   bool   `msg:"new_if_missing"`
}

// Attached reports back the state at attach time: window+tab layout
// so any client can reconstruct the user's intended UI shape no
// matter which machine they attached from. Tabs is the flat list of
// every tab in the session (each tab also appears in exactly one
// Window's TabIDs list).
type Attached struct {
	SessionName string       `msg:"session_name"`
	Windows     []WindowInfo `msg:"windows"`
	Tabs        []TabInfo    `msg:"tabs"`
	FocusedTabID uint32      `msg:"focused_tab_id"` // app-wide focused tab
}

// WindowInfo describes a top-level UI window — a grouping of tabs.
// PosX/PosY/Width/Height are hints the UI uses on restore; under
// Wayland the compositor may ignore them but the UI still tries.
// TabIDs is the tab-bar order (left-to-right). FocusedTabID is the
// tab that's currently in front within this window.
type WindowInfo struct {
	ID           uint32  `msg:"id"`
	PosX         int32   `msg:"pos_x"`
	PosY         int32   `msg:"pos_y"`
	Width        int32   `msg:"width"`
	Height       int32   `msg:"height"`
	TabIDs       []uint32 `msg:"tab_ids"`
	FocusedTabID uint32  `msg:"focused_tab_id"`
}

// TabInfo is a slim summary of a tab sent at attach time. Full cell
// state comes via CellFull after the client requests it.
type TabInfo struct {
	ID    uint32 `msg:"id"`
	Title string `msg:"title"`
	Cols  uint16 `msg:"cols"`
	Rows  uint16 `msg:"rows"`
}

// Detach asks the server to release this connection without killing
// any tabs. The session and its tabs persist.
type Detach struct{}

// Error reports a protocol-level error. The server may close the
// connection after sending one of these.
type Error struct {
	Code    uint16 `msg:"code"`
	Message string `msg:"message"`
}

// TabCreate asks the daemon to spawn a new tab in the attached
// session. WindowID picks which window the tab joins (0 means "the
// session's default/first window"). Cwd/Command optional.
type TabCreate struct {
	WindowID uint32 `msg:"window_id,omitempty"`
	Cols     uint16 `msg:"cols"`
	Rows     uint16 `msg:"rows"`
	Cwd      string `msg:"cwd,omitempty"`
	Command  string `msg:"command,omitempty"`
}

// TabClose asks the daemon to close a tab. Daemon kills the child
// and removes the tab from the session.
type TabClose struct {
	ID uint32 `msg:"id"`
}

// TabFocus tells the daemon which tab the user is looking at. The
// daemon may use this to prioritize cell-diff frame delivery for
// the focused tab over background tabs.
type TabFocus struct {
	ID uint32 `msg:"id"`
}

// TabCreate (revised) — now optionally attaches the new tab to a
// specific window. Empty WindowID means "default window" / "session's
// only window".
type TabCreatedInfo TabInfo

// TabCreated confirms a newly-spawned tab's assigned ID + initial
// dimensions, and which window it landed in.
type TabCreated struct {
	Info     TabInfo `msg:"info"`
	WindowID uint32  `msg:"window_id"`
}

// WindowCreate asks the daemon to register a new logical UI window
// in the session. The window has no tabs yet — the UI follows up
// with TabCreate (or MoveTab) to populate it.
type WindowCreate struct {
	PosX   int32 `msg:"pos_x,omitempty"`
	PosY   int32 `msg:"pos_y,omitempty"`
	Width  int32 `msg:"width,omitempty"`
	Height int32 `msg:"height,omitempty"`
}

// WindowCreated confirms a new window's assigned ID.
type WindowCreated struct {
	Info WindowInfo `msg:"info"`
}

// WindowClose tears down a window. Tabs in the window get reassigned
// to another window in the session (the first one); if it was the
// last window, the tabs are moved into a freshly-created default
// window so they stay accessible from a future attach.
type WindowClose struct {
	ID uint32 `msg:"id"`
}

// WindowMoveTab moves a tab from one window to another. Index is
// the position within the destination window's tab bar (left-to-
// right). Negative or out-of-range index appends.
type WindowMoveTab struct {
	TabID         uint32 `msg:"tab_id"`
	ToWindowID    uint32 `msg:"to_window_id"`
	Index         int32  `msg:"index"`
}

// WindowGeometry reports a UI window's new position/size. The
// daemon stores this so a roaming UI can re-open the window in
// the same place.
type WindowGeometry struct {
	ID     uint32 `msg:"id"`
	PosX   int32  `msg:"pos_x"`
	PosY   int32  `msg:"pos_y"`
	Width  int32  `msg:"width"`
	Height int32  `msg:"height"`
}

// WindowFocusTab reports which tab is in front within a given
// window. Per-window focus (TabFocus is app-wide).
type WindowFocusTab struct {
	WindowID uint32 `msg:"window_id"`
	TabID    uint32 `msg:"tab_id"`
}

// Resize tells the daemon the user resized the tab's grid. Daemon
// resizes the PTY winsize and the SafeEmulator screen.
type Resize struct {
	ID   uint32 `msg:"id"`
	Cols uint16 `msg:"cols"`
	Rows uint16 `msg:"rows"`
}

// InputBytes carries raw bytes to write to a tab's PTY. Used for
// keystrokes that the UI already translated into VT escape
// sequences (see internal/input).
type InputBytes struct {
	ID    uint32 `msg:"id"`
	Bytes []byte `msg:"bytes"`
}

// InputPaste carries pasted text — same destination as InputBytes
// but framed separately so the daemon can decide whether to wrap
// in bracketed-paste escapes (CSI ?200 h / CSI ?201 h) based on
// the tab's current bracketed-paste mode.
type InputPaste struct {
	ID    uint32 `msg:"id"`
	Bytes []byte `msg:"bytes"`
}

// InputImage carries a pasted image blob — bytes are the raw image
// (PNG, JPEG, etc., MIME identifies which). Daemon writes the bytes
// to a temp file on the daemon-side filesystem (so Claude Code or
// any other PTY child can read it natively without base64 / OSC
// shenanigans) and types the path into the PTY as if the user had
// typed it. This is the original motivation for the daemon arc:
// pasting screenshots into a remote Claude Code session "just
// works" without the SSH escape-sequence brittleness that breaks
// over flaky links.
//
// MIME helps the daemon pick a sensible file extension. Filename
// (optional) is a UI hint — daemon may incorporate it into the
// temp filename but appends a unique suffix to avoid collisions.
type InputImage struct {
	ID       uint32 `msg:"id"`
	MIME     string `msg:"mime"`
	Filename string `msg:"filename,omitempty"`
	Bytes    []byte `msg:"bytes"`
}

// ClipboardData carries clipboard text. The direction tells the
// receiver what to do with it:
//
//   - Client → Server: "this is what the user just put in their
//     local clipboard." Daemon makes it available to PTY children
//     via OSC 52 reads.
//
//   - Server → Client: "an app on the daemon side wrote to the
//     clipboard via OSC 52 set." Client puts it on the local
//     system clipboard.
//
// Phase 3 implements only the client→server direction; the
// server→client OSC 52 set path lands when we wire up OSC 52
// passthrough in the vt emulator (Phase 4+).
type ClipboardData struct {
	Text     string `msg:"text"`
	MIME     string `msg:"mime,omitempty"` // default "text/plain"
}

// Cell is the wire representation of a single grid cell. Mirrors
// ultraviolet.Cell but with style packed for compactness.
//
// Style is a 32-bit packed value:
//   bits  0..7  attribute flags (bold/italic/underline/etc.)
//   bits  8..19 fg index (0xFFF means "RGB follows")
//   bits 20..31 bg index (0xFFF means "RGB follows")
//
// When fg or bg uses the "RGB follows" sentinel, the cell is
// serialized with RGB fields populated. 99% of terminal output uses
// palette colors so the common case is 4 bytes + content.
type Cell struct {
	Content string `msg:"c"`            // single grapheme cluster; "" treated as " "
	Style   uint32 `msg:"s"`            // packed (see above)
	Width   uint8  `msg:"w"`            // 0 (combining), 1 (normal), 2 (wide)
	FgRGB   uint32 `msg:"fg,omitempty"` // present only when style fg-idx == 0xFFF
	BgRGB   uint32 `msg:"bg,omitempty"` // present only when style bg-idx == 0xFFF
}

// CellDiffEntry is one cell + position. CellDiff messages carry a
// slice of these.
type CellDiffEntry struct {
	Row  uint16 `msg:"r"`
	Col  uint16 `msg:"c"`
	Cell Cell   `msg:"v"`
}

// CellDiff carries cells that changed since the last frame for a
// given tab. Only the changed cells; empty diffs are not sent.
type CellDiff struct {
	ID    uint32          `msg:"id"`
	Cells []CellDiffEntry `msg:"cells"`
}

// CellFull is the entire visible grid for a tab. Sent on initial
// attach and after any forced resync. Rows are top-to-bottom, each
// row is left-to-right cells.
type CellFull struct {
	ID   uint32   `msg:"id"`
	Cols uint16   `msg:"cols"`
	Rows uint16   `msg:"rows"`
	Grid [][]Cell `msg:"grid"`
}

// Cursor reports the cursor position for a tab. Sent whenever the
// cursor moves OR its visibility/style changes.
//
// Style uses the vt shape enum (0=block, 1=underline, 2=bar) — NOT
// the raw DECSCUSR 1..6 codes. Blink carries the blink flag
// separately (DECSCUSR's odd/even convention is decoded on the
// daemon side). StyleSet reports whether the foreground app
// explicitly chose a cursor style via DECSCUSR: when false the
// client should fall back to its own configured cursor preference
// rather than the (block) default.
type Cursor struct {
	ID       uint32 `msg:"id"`
	Row      uint16 `msg:"row"`
	Col      uint16 `msg:"col"`
	Visible  bool   `msg:"visible"`
	Style    uint8  `msg:"style"` // vt enum: 0=block, 1=underline, 2=bar
	Blink    bool   `msg:"blink"`
	StyleSet bool   `msg:"style_set"`
}

// Title carries an OSC 0/1/2 title update for a tab.
type Title struct {
	ID    uint32 `msg:"id"`
	Title string `msg:"title"`
}

// Bell signals the terminal bell (BEL = 0x07) was received.
type Bell struct {
	ID uint32 `msg:"id"`
}

// ChildExit reports that a tab's PTY child process exited. The tab
// itself stays in the daemon's tab list until explicitly closed (so
// the user can scroll the final output and decide).
type ChildExit struct {
	ID       uint32 `msg:"id"`
	ExitCode int32  `msg:"exit_code"`
}

// TabState carries slow-changing per-tab metadata: the foreground
// process's working directory, its command name (for tab-title
// fallback), and DECCKM (app cursor mode, for keyboard translation).
// Daemon pushes proactively rather than answering per-request:
//
//   - once at attach time (so the client has values immediately)
//   - whenever DECCKM toggles (response is immediate; keyboards
//     need this to be correct)
//   - on a slow timer when the foreground process changes (so the
//     title bar updates within a beat of `vim` starting)
//
// Push-not-pull avoids the per-frame round-trip the GUI would
// otherwise need for every renderable change.
type TabState struct {
	ID                    uint32 `msg:"id"`
	CWD                   string `msg:"cwd,omitempty"`
	ForegroundProcessName string `msg:"fg_proc,omitempty"`
	AppCursorMode         bool   `msg:"app_cursor"`
	// Title is the OSC 0/2 title the foreground app set. Pushed
	// here (rather than a separate MsgTitle stream) because it
	// already shares the "slow-changing per-tab metadata" cadence
	// with cwd/fg_proc/app_cursor. Empty = no title set yet.
	Title string `msg:"title,omitempty"`
}

// ClearScrollback asks the daemon to drop the tab's scrollback ring.
// Issued by Ctrl+L's "hard clear" path. The daemon clears the
// emulator's scrollback in addition to any disk-backed extension.
type ClearScrollback struct {
	ID uint32 `msg:"id"`
}

// ScrollbackCleared notifies a client that the tab's scrollback was
// dropped (either by this client, another attached client, or the
// daemon itself). Receivers should drop their local scrollback
// mirror so the GUI doesn't keep showing stale history. The
// scrollback append index resets to 0 on the daemon side, so the
// next ScrollbackAppend will start from 0 too.
type ScrollbackCleared struct {
	ID uint32 `msg:"id"`
}

// ScrollbackAppend ships rows that just rolled off the top of the
// visible viewport into the daemon's scrollback ring. Client appends
// to its own scrollback buffer so scrolling up shows history.
//
// BaseIdx is the absolute scrollback row index of the first row in
// Rows (0 = oldest line ever scrolled). Lets the client detect
// gaps (e.g. after a reattach where the daemon's scrollback already
// has rows the client never saw) and either request a backfill or
// just skip ahead.
//
// Rows are oldest-first (Rows[0] is older than Rows[1]). Daemons
// can batch — sending several rows in one frame is allowed when
// multiple lines scroll off in quick succession (e.g. `find /`
// output).
type ScrollbackAppend struct {
	ID      uint32   `msg:"id"`
	BaseIdx uint32   `msg:"base_idx"`
	Rows    [][]Cell `msg:"rows"`
}
