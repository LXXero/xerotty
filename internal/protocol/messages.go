package protocol

//go:generate msgp -unexported=false

// ProtocolVersion is the wire-format version this build of xerotty
// and xerottyd speak. Bump on any backwards-incompatible change to
// any of the message structs below. Minor field additions are NOT
// breaking — msgpack lets newer servers send fields older clients
// ignore.
const ProtocolVersion uint16 = 1

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

	MsgResize            MsgType = 20 // client → server (per-tab cols/rows)
	MsgInputBytes        MsgType = 21 // client → server: raw bytes for PTY
	MsgInputPaste        MsgType = 22 // client → server: paste (may be longer)

	MsgCellFull          MsgType = 30 // server → client: full grid (initial / resync)
	MsgCellDiff          MsgType = 31 // server → client: changed cells
	MsgCursor            MsgType = 32 // server → client: cursor position/style
	MsgTitle             MsgType = 33 // server → client: tab title (from OSC)
	MsgBell              MsgType = 34 // server → client
	MsgChildExit         MsgType = 35 // server → client: PTY child exited
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
type HelloAck struct {
	ServerVersion uint16 `msg:"server_version"`
	ServerID      string `msg:"server_id"`        // e.g. hostname + daemon pid
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

// Attached reports back the state at attach time: tab list with
// current dimensions and titles so the client can paint the tab bar.
// The client follows up by requesting CellFull for the focused tab.
type Attached struct {
	SessionName string    `msg:"session_name"`
	Tabs        []TabInfo `msg:"tabs"`
	FocusedID   uint32    `msg:"focused_id"`
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
// session. Cwd/Command optional.
type TabCreate struct {
	Cols    uint16 `msg:"cols"`
	Rows    uint16 `msg:"rows"`
	Cwd     string `msg:"cwd,omitempty"`
	Command string `msg:"command,omitempty"`
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

// TabCreated confirms a newly-spawned tab's assigned ID + initial
// dimensions.
type TabCreated struct {
	Info TabInfo `msg:"info"`
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
type Cursor struct {
	ID      uint32 `msg:"id"`
	Row     uint16 `msg:"row"`
	Col     uint16 `msg:"col"`
	Visible bool   `msg:"visible"`
	Style   uint8  `msg:"style"` // 0=block, 1=underline, 2=bar
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
