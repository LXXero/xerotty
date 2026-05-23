package terminal

import (
	uv "github.com/charmbracelet/ultraviolet"
	vt "github.com/charmbracelet/x/vt"

	"github.com/LXXero/xerotty/internal/config"
)

// Source is the abstraction over "thing that owns a terminal grid +
// PTY-like input sink." Two impls exist:
//
//   - *Terminal — owns the PTY in-process. Default for the GUI when
//     no daemon is configured.
//   - daemonsource.Source — proxies all reads/writes over the xerotty
//     wire protocol to a `xerotty serve` daemon. The grid state is
//     mirrored locally into a *vt.SafeEmulator so selection,
//     scrollback, search, and link detection keep working through
//     the same emulator API the GUI already uses.
//
// Anything the GUI code needs from a terminal-backed tab MUST appear
// here. Adding a method to Source means both impls have to support
// it; that's by design — keep the surface small.
type Source interface {
	// Emulator returns the *vt.SafeEmulator the GUI reads grid +
	// cursor + scrollback state from. For PTYSource this is the live
	// emulator; for DaemonSource it's a shadow emulator kept in
	// sync by inbound CellFull/CellDiff/Cursor frames. Named
	// Emulator() (not Emu()) so the existing exported `Emu` field
	// on *Terminal doesn't conflict.
	Emulator() *vt.SafeEmulator

	// Write sends raw bytes to the PTY (keystrokes, escape sequences
	// constructed by internal/input). io.Writer-shaped so callers
	// that already speak the standard signature drop in unchanged.
	Write(p []byte) (int, error)

	// Resize changes the PTY winsize and the emulator's grid dims.
	// Safe to call from any goroutine.
	Resize(cols, rows int)

	// Close kills the child + releases resources. Idempotent.
	// Use for "user explicitly closed this tab" — caller's intent
	// is to end the session, not just to stop viewing it.
	Close()

	// Detach releases this client's view of the tab WITHOUT killing
	// the underlying session. For PTYSource (no daemon) this is
	// equivalent to Close (PTY can't outlive the process); for
	// DaemonSource it unregisters from the hub but keeps the
	// daemon-side tab alive so a future Adopt or attach can pick
	// it back up. Use for "GUI window/app closing" so reopening
	// xerotty finds the same tabs waiting.
	Detach()

	// IsClosed reports whether the child process has exited (or the
	// remote tab has been torn down).
	IsClosed() bool

	// ChildExitCode returns the child's exit code. -1 = still
	// running / unknown. Named ChildExitCode() (not ExitCode()) so
	// it doesn't conflict with the existing exported `ExitCode`
	// field on *Terminal.
	ChildExitCode() int

	// Paste sends `text` to the PTY, wrapping with bracketed-paste
	// markers when the foreground app enabled DECSET 2004.
	Paste(text string)

	// ScrollbackLen returns the number of rows currently in
	// scrollback (above the visible viewport).
	ScrollbackLen() int

	// AppCursorMode reports whether DECCKM is set (controls whether
	// arrow keys emit ESC O X or ESC [ X).
	AppCursorMode() bool

	// GetCWD returns the foreground process group's working
	// directory, or "" if it can't be determined. Used so "New Tab"
	// can inherit the current dir.
	GetCWD() string

	// ForegroundProcessName returns the foreground process group's
	// command name (e.g. "vim", "less", "ssh"). Used as a tab title
	// fallback when no OSC has fired.
	ForegroundProcessName() string

	// DataChan returns a channel that gets a signal whenever new
	// data has updated the grid. Buffered cap=1 — coalesces bursts.
	DataChan() <-chan struct{}

	// SetOnTitle registers a callback fired on OSC 0/2 title changes.
	// Pass nil to clear.
	SetOnTitle(fn func(string))

	// SetOnBell registers a callback fired when the terminal bell
	// (BEL = 0x07) is received. Pass nil to clear. Implementations
	// route to local audio/visual hooks; GUIs typically flash the
	// tab.
	SetOnBell(fn func())

	// Renderer-side view: subset of EmulatorView the renderer uses.
	// *Terminal already exposes these (they pass through to the
	// emulator + the disk-scrollback ring); DaemonSource implements
	// them against the shadow emulator + scrollback view kept in
	// sync by the wire protocol.
	//
	// CellAt / ScrollbackCellAt may return pointers into live
	// emulator memory — fine for the GUI's per-frame renderer
	// (tearing during heavy output is imperceptible at 60fps) but
	// NOT safe for concurrent readers that race with the writer
	// goroutine. Callers needing a consistent snapshot should use
	// SnapshotViewport / SnapshotScrollbackRange instead.
	Width() int
	Height() int
	CellAt(col, row int) *uv.Cell
	ScrollbackCellAt(col, row int) *uv.Cell

	// SnapshotViewport returns a consistent copy of the visible
	// grid (cells are values, not pointers). Safe to read fields
	// after the call returns. Used by daemon publish path, MCP
	// screen reads, and any test that walks cells while the
	// emulator is being written.
	SnapshotViewport() [][]uv.Cell

	// SnapshotScrollbackRange returns rows [from, to) as a
	// consistent copy. Indices are absolute (0 = oldest).
	SnapshotScrollbackRange(from, to int) [][]uv.Cell

	// SetScrollbackFromConfig re-applies scrollback settings (mode,
	// line limit) without restarting the source. Called from the
	// prefs-apply path so live tabs pick up new settings. Daemon-
	// backed sources may translate this into a config push over the
	// wire; PTY-backed sources reconfigure their own ring.
	SetScrollbackFromConfig(cfg *config.Config)

	// ClearScrollback drops the tab's scrollback history. For PTY-
	// backed sources it clears the emulator's ring + any disk-backed
	// scrollback. For DaemonSource it ships MsgClearScrollback to
	// the daemon AND drops the local shadow ring so the GUI sees
	// the result immediately instead of waiting for a round-trip.
	ClearScrollback()

	// PasteImage delivers a clipboard image to the tab. DaemonSource
	// ships MsgInputImage so the daemon writes the bytes to a
	// daemon-side temp file and types the path into the PTY (works
	// over SSH without escape-sequence pain). PTYSource writes the
	// bytes to a local temp file and pastes the resulting path so
	// the behavior is consistent across modes.
	//
	// mime is the canonical MIME type (e.g. "image/png"). filename
	// is a hint for the temp prefix; sanitized by the receiver.
	PasteImage(mime, filename string, data []byte) error
}

// Compile-time assertion that *Terminal satisfies Source. If you
// add a method to Source, fix it in terminal.go (PTYSource shim
// methods at the bottom of that file).
var _ Source = (*Terminal)(nil)
