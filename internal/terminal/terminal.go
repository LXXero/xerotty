package terminal

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/LXXero/xerotty/internal/config"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// Wake is called from PTY reader goroutines after new data arrives so
// the main loop can break out of its WaitEventTimeout sleep early and
// render. App.Run() sets this to a function that pushes an SDL wake
// event. nil means the main loop is poll-based (cimgui-go default) and
// the DataCh send alone is enough.
var Wake func()

// Terminal wraps a SafeEmulator + PTY + reader goroutines for one tab.
type Terminal struct {
	Emu      *vt.SafeEmulator
	ptmx     *os.File
	cmd      *exec.Cmd
	DataCh   chan struct{} // signals new data for rendering (buffered, cap 1)
	OnTitle  func(string)  // called when OSC 0/2 sets window title

	// OnChildExit fires from the waitChild goroutine the instant
	// the PTY child reaps. Caller gets the exit code; the daemon
	// uses this to ship a MsgChildExit to attached wire clients,
	// the GUI uses it to drive the on-child-exit policy. Set via
	// SetOnChildExit so the callback swap is mutex-guarded.
	OnChildExit func(int)
	cols        int
	rows        int
	closed      bool // Close() has run and released resources
	childExited bool // child process has exited (Wait() returned)
	ExitCode    int  // exit code of child process (-1 = still running or unknown)
	mu          sync.Mutex
	closeOnce   sync.Once
	done        chan struct{}

	// appCursor tracks DECCKM (DEC private mode 1). When set, arrow keys
	// emit `ESC O X` instead of `ESC [ X` so pagers (less, git diff via
	// less, vim insert-mode movement) recognize them. Flipped on the PTY
	// reader goroutine via the EnableMode/DisableMode emulator callbacks;
	// read from the UI goroutine when translating key events.
	appCursor atomic.Bool

	// sgrMouse tracks SGR mouse mode (1006). When set, mouse events are
	// reported in SGR format (CSI < ... m) instead of the default format.
	sgrMouse atomic.Bool

	// bracketedPaste tracks bracketed paste mode (2004). When set, paste
	// events are wrapped in CSI ? 200 h and CSI ? 201 h.
	bracketedPaste atomic.Bool

	// disk-backed scrollback state, used only when the configured
	// scrollback Mode is "unlimited". When vt's in-mem scrollback
	// grows past 2*liveWindow, the oldest (memLen - liveWindow)
	// lines are evicted to disk in one batch. Lines live in EXACTLY
	// ONE place — disk for indices 0..disk.Len()-1, vt's in-mem
	// ring for the rest.
	disk       *DiskScrollback
	liveWindow int // soft cap on in-mem scrollback when disk-backed
}

// New creates a terminal with the given dimensions and starts the shell.
func New(cfg *config.Config, cols, rows int, cwd string) (*Terminal, error) {
	ptmx, cmd, err := spawnPTY(cfg, uint16(cols), uint16(rows), cwd)
	if err != nil {
		return nil, err
	}

	emu := vt.NewSafeEmulator(cols, rows)

	t := &Terminal{
		Emu:      emu,
		ptmx:     ptmx,
		cmd:      cmd,
		DataCh:   make(chan struct{}, 1),
		cols:     cols,
		rows:     rows,
		ExitCode: -1,
		done:     make(chan struct{}),
	}
	t.applyScrollbackConfig(cfg)

	emu.Emulator.SetCallbacks(vt.Callbacks{
		Title: func(title string) {
			if t.OnTitle != nil {
				t.OnTitle(title)
			}
		},
		EnableMode: func(mode ansi.Mode) {
			switch mode {
			case ansi.ModeCursorKeys:
				t.appCursor.Store(true)
			case ansi.ModeMouseExtSgr:
				t.sgrMouse.Store(true)
			case ansi.ModeBracketedPaste:
				t.bracketedPaste.Store(true)
			}
		},
		DisableMode: func(mode ansi.Mode) {
			switch mode {
			case ansi.ModeCursorKeys:
				t.appCursor.Store(false)
			case ansi.ModeMouseExtSgr:
				t.sgrMouse.Store(false)
			case ansi.ModeBracketedPaste:
				t.bracketedPaste.Store(false)
			}
		},
	})

	// PTY Reader goroutine: PTY → SafeEmulator
	go t.readPTY()
	// Emulator Reader goroutine: SafeEmulator → PTY (device responses)
	go t.readEmu()
	// Wait for child process exit
	go t.waitChild()

	return t, nil
}

// liveWindowDefault is how many recent lines we keep in vt's
// in-memory ring under unlimited mode before trimming to disk.
// 4096 ≈ 100 page-fulls of an 80x40 terminal — enough that the
// active scrollback experience never hits disk for typical use.
const liveWindowDefault = 4096

// applyScrollbackConfig sets the emulator's scrollback buffer size
// from config. Mode == "unlimited" maps to a very large cap (no
// realistic terminal session hits 2 billion lines) and spills oldest
// lines to disk via NewDiskScrollback; "memory" (or any other value)
// uses cfg.Scrollback.Lines as a finite ring buffer with no disk
// backing.
//
// Caller must hold t.mu (or be in a path where racing is OK, e.g.
// during construction before goroutines start).
func (t *Terminal) applyScrollbackConfig(cfg *config.Config) {
	if t.Emu == nil {
		return
	}
	switch cfg.Scrollback.Mode {
	case "unlimited":
		// Keep vt's MaxLines high so it doesn't auto-evict; we
		// manage trimming ourselves after mirroring to disk.
		t.Emu.SetScrollbackSize(int(^uint(0) >> 1))
		t.liveWindow = liveWindowDefault
		if t.disk == nil {
			if d, err := NewDiskScrollback(); err == nil {
				t.disk = d
			}
			// If disk creation fails we silently fall back to
			// memory-only "unlimited" — same UX as before this
			// patch. Practically rare (/tmp not writable etc.).
		}
	default:
		lines := cfg.Scrollback.Lines
		if lines < 0 {
			lines = 0
		}
		t.Emu.SetScrollbackSize(lines)
		t.liveWindow = 0
		if t.disk != nil {
			_ = t.disk.Close()
			t.disk = nil
		}
	}
}

// SetScrollbackFromConfig re-applies the scrollback Mode + Lines
// config to a running terminal. Called from the preferences-apply
// path when the user changes scrollback settings without quitting.
func (t *Terminal) SetScrollbackFromConfig(cfg *config.Config) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.applyScrollbackConfig(cfg)
}

// mirrorScrollback evicts old in-memory scrollback lines to disk
// whenever vt's in-mem count exceeds 2x liveWindow. Strict
// eviction: a line either lives in vt's in-mem ring OR on disk,
// never both. No-op if disk isn't initialized (not unlimited mode).
//
// Invariant:
//   - lines [0 .. disk.Len()-1]                  live ONLY on disk
//   - lines [disk.Len() .. disk.Len()+memLen-1]  live ONLY in vt
//   - total ScrollbackLen() = disk.Len() + memLen
//
// 2x hysteresis on the trim point avoids trim-on-every-line thrash:
// the in-mem window grows up to 2*liveWindow, then we evict the
// oldest (memLen - liveWindow) lines in one batch.
func (t *Terminal) mirrorScrollback() {
	t.mu.Lock()
	disk := t.disk
	liveWindow := t.liveWindow
	t.mu.Unlock()

	if disk == nil || liveWindow <= 0 {
		return
	}

	memLen := t.Emu.ScrollbackLen()
	if memLen <= liveWindow*2 {
		return
	}
	dropCount := memLen - liveWindow

	sb := t.Emu.Scrollback()
	if sb == nil {
		return
	}
	lines := sb.Lines()
	// Defensive: lines should always have at least dropCount entries
	// (we just read memLen and dropCount = memLen - liveWindow), but
	// guard against a concurrent ScrollbackLen change between read
	// and Lines().
	if dropCount > len(lines) {
		dropCount = len(lines)
	}
	for i := 0; i < dropCount; i++ {
		if err := disk.Append(lines[i]); err != nil {
			// Disk write failed mid-eviction — stop and let vt
			// keep these lines in memory. Next call retries
			// from this point. Partial-eviction is safe: the
			// lines we DID write are gone from vt only after
			// SetScrollbackSize runs below.
			dropCount = i
			break
		}
	}
	if dropCount <= 0 {
		return
	}
	// Drop the oldest dropCount lines from vt. SetScrollbackSize
	// shrink path discards exactly the prefix we already wrote.
	t.Emu.SetScrollbackSize(liveWindow)
	t.Emu.SetScrollbackSize(int(^uint(0) >> 1))
}

// ScrollbackLen returns total scrollback length (disk + in-memory).
// Used by the renderer and scroll-offset clamping logic; replaces
// callers that used to read Emu.ScrollbackLen() directly.
func (t *Terminal) ScrollbackLen() int {
	t.mu.Lock()
	disk := t.disk
	t.mu.Unlock()
	memLen := t.Emu.ScrollbackLen()
	if disk == nil {
		return memLen
	}
	return disk.Len() + memLen
}

// ScrollbackCellAt returns the cell at (col, row) in scrollback,
// reading from disk for rows below disk.Len() and from vt's
// in-memory ring for rows above. Returns nil for out-of-range or
// past-end-of-line indices (renderer treats as empty cell).
func (t *Terminal) ScrollbackCellAt(col, row int) *uv.Cell {
	t.mu.Lock()
	disk := t.disk
	t.mu.Unlock()

	if disk == nil {
		return t.Emu.ScrollbackCellAt(col, row)
	}
	diskLen := disk.Len()
	if row < diskLen {
		line := disk.LineAt(row)
		if line == nil || col < 0 || col >= len(line) {
			return nil
		}
		c := line[col]
		return &c
	}
	return t.Emu.ScrollbackCellAt(col, row-diskLen)
}

// Width / Height / CellAt / CursorPosition pass through to the
// emulator. Defined here so callers can use *terminal.Terminal as
// the renderer.EmulatorView interface without changing every site
// to dig through .Emu.
func (t *Terminal) Width() int                  { return t.Emu.Width() }
func (t *Terminal) Height() int                 { return t.Emu.Height() }
func (t *Terminal) CellAt(col, row int) *uv.Cell { return t.Emu.CellAt(col, row) }
func (t *Terminal) CursorPosition() uv.Position { return t.Emu.CursorPosition() }

// Write sends data to the PTY (keyboard input).
func (t *Terminal) Write(p []byte) (int, error) {
	return t.ptmx.Write(p)
}

// Resize updates the PTY and emulator dimensions.
func (t *Terminal) Resize(cols, rows int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cols = cols
	t.rows = rows
	_ = pty.Setsize(t.ptmx, &pty.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
	})
	t.Emu.Resize(cols, rows)
}

// Close shuts down the terminal: kills the child, closes the PTY, stops
// goroutines. Idempotent via sync.Once so the natural-exit path
// (waitChild already returned, child reaped) still releases the PTY fd
// when the user later closes the tab.
func (t *Terminal) Close() {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		t.mu.Unlock()

		close(t.done)
		if t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
		_ = t.ptmx.Close()
		// waitChild goroutine handles cmd.Wait()
	})
}

// AppCursorMode reports whether DECCKM is currently set, meaning arrow
// keys should be sent as `ESC O X` instead of `ESC [ X`.
func (t *Terminal) AppCursorMode() bool {
	return t.appCursor.Load()
}

// Source interface shims. These exist so *Terminal satisfies
// terminal.Source without renaming the exported Emu / ExitCode /
// DataCh fields (which a lot of internal code reads directly).
//
// Method names deliberately differ from the field names to dodge
// the field-vs-method conflict Go would otherwise flag.

// Emulator returns the underlying vt.SafeEmulator. Same value as the
// public Emu field; method form is what terminal.Source requires.
func (t *Terminal) Emulator() *vt.SafeEmulator { return t.Emu }

// ChildExitCode returns the child process's exit code (-1 if still
// running / unknown). Method form of the public ExitCode field for
// Source-interface use.
func (t *Terminal) ChildExitCode() int { return t.ExitCode }

// DataChan returns the dirty-tab signal channel. Method form of the
// public DataCh field for Source-interface use.
func (t *Terminal) DataChan() <-chan struct{} { return t.DataCh }

// SetOnTitle registers a callback fired on OSC 0/2 title changes.
// Pass nil to clear. Holds t.mu so callers swapping the callback
// from a different goroutine don't race the readPTY goroutine that
// invokes it.
func (t *Terminal) SetOnTitle(fn func(string)) {
	t.mu.Lock()
	t.OnTitle = fn
	t.mu.Unlock()
}

// SetOnChildExit registers a callback fired the instant the PTY
// child exits. May fire immediately if the child has already
// exited by the time this is called (rare but possible — e.g. a
// shell that crashes during init). Pass nil to clear.
func (t *Terminal) SetOnChildExit(fn func(int)) {
	t.mu.Lock()
	t.OnChildExit = fn
	alreadyExited := t.childExited
	code := t.ExitCode
	t.mu.Unlock()
	if alreadyExited && fn != nil {
		fn(code)
	}
}

// IsClosed reports whether this Terminal is no longer usable — either
// because Close() ran or because the child process exited on its own.
// Used by tabs.CheckClosed to mark a tab as "Closed" in the UI so the
// on-child-exit policy (close/hold) can apply.
func (t *Terminal) IsClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed || t.childExited
}

// waitChild waits for the child process to exit and records its exit code.
// Sets childExited (not closed) so a later Close() call still runs the
// cleanup once — otherwise the PTY fd, done channel, and any other
// per-tab resources would leak whenever a tab exits naturally before the
// user closes it explicitly.
//
// Fires t.OnChildExit (under the mutex, after state is set) so any
// observer — the daemon's publishLoop, the GUI's tab manager, etc.
// — gets notified synchronously with the state transition. Without
// this, attached clients in daemon mode never learn the shell died
// and the tab hangs forever (no DataCh signal because the PTY
// reader has nothing to read; no MsgChildExit because nobody sent
// it).
func (t *Terminal) waitChild() {
	err := t.cmd.Wait()
	t.mu.Lock()
	if err == nil {
		t.ExitCode = 0
	} else if exitErr, ok := err.(*exec.ExitError); ok {
		t.ExitCode = exitErr.ExitCode()
	} else {
		t.ExitCode = 1
	}
	t.childExited = true
	cb := t.OnChildExit
	code := t.ExitCode
	t.mu.Unlock()
	if cb != nil {
		cb(code)
	}
}

// readPTY reads from the PTY and writes to the SafeEmulator.
func (t *Terminal) readPTY() {
	buf := make([]byte, 32*1024)
	// OSC pre-processor state. Carries over across Read calls in case an
	// OSC sequence spans buffer boundaries.
	var oscBuf []byte
	inOSC := false
	for {
		select {
		case <-t.done:
			return
		default:
		}

		n, err := t.ptmx.Read(buf)
		if n > 0 {
			cleaned, newOSCBuf, newInOSC := t.preprocessOSC(buf[:n], oscBuf, inOSC)
			oscBuf = newOSCBuf
			inOSC = newInOSC
			if len(cleaned) > 0 {
				t.Emu.Write(cleaned)
				// Mirror new scrollback lines to disk in unlimited
				// mode. No-op otherwise. Runs on this goroutine so
				// the mirror always sees writes in PTY-arrival order.
				t.mirrorScrollback()
			}
			select {
			case t.DataCh <- struct{}{}:
			default:
			}
			if Wake != nil {
				Wake()
			}
		}
		if err != nil {
			if err != io.EOF {
				// PTY closed or error — mark terminal as done
			}
			return
		}
	}
}

// preprocessOSC intercepts OSC sequences before they reach the vt emulator,
// because charm/x/ansi's parser misinterprets UTF-8 continuation byte 0x9c
// as the C1 String Terminator. That breaks any OSC body containing
// multi-byte UTF-8 (e.g. claude's window title "✳ Claude Code"): the title
// is truncated to the first byte and the remaining bytes leak onto the
// screen as plain text. We scan the byte stream, dispatch known OSC
// sequences directly, and strip them from what gets sent to vt.
//
// State (oscBuf, inOSC) carries across Read calls in case a sequence spans
// buffer boundaries. Returns the cleaned-of-OSC bytes plus the new state.
func (t *Terminal) preprocessOSC(in, oscBufIn []byte, inOSCIn bool) ([]byte, []byte, bool) {
	out := make([]byte, 0, len(in))
	oscBuf := oscBufIn
	inOSC := inOSCIn
	for i := 0; i < len(in); i++ {
		b := in[i]
		if !inOSC {
			// Recognize only the 7-bit OSC introducer (ESC ']'). The 8-bit
			// form 0x9d is also a valid UTF-8 continuation byte (e.g. the
			// box-drawing char U+255D ╝ is \xe2\x95\x9d), so matching it
			// would incorrectly enter OSC mode mid-glyph and swallow the
			// rest of the rendering. Modern apps emit 7-bit forms anyway.
			if b == 0x1b && i+1 < len(in) && in[i+1] == ']' {
				inOSC = true
				oscBuf = oscBuf[:0]
				i++ // skip the ']'
				continue
			}
			out = append(out, b)
			continue
		}
		// Inside OSC body. Terminators: BEL (0x07), or ESC '\\' (string
		// terminator). We deliberately do NOT treat 0x9c as a terminator
		// here — that's the byte that breaks vt — and pass it through as
		// part of the body so the UTF-8 sequence stays intact.
		if b == 0x07 {
			t.dispatchOSC(oscBuf)
			oscBuf = oscBuf[:0]
			inOSC = false
			continue
		}
		if b == 0x1b && i+1 < len(in) && in[i+1] == '\\' {
			t.dispatchOSC(oscBuf)
			oscBuf = oscBuf[:0]
			inOSC = false
			i++ // skip the '\\'
			continue
		}
		oscBuf = append(oscBuf, b)
	}
	return out, oscBuf, inOSC
}

// dispatchOSC handles a complete OSC body (between OSC introducer and
// terminator). Currently only OSC 0/1/2 (window title / icon name) are
// handled — that's the case that breaks visibly. Other OSC commands are
// silently dropped, which matches what an unsupported terminal would do.
func (t *Terminal) dispatchOSC(body []byte) {
	semi := -1
	for i, b := range body {
		if b == ';' {
			semi = i
			break
		}
	}
	if semi <= 0 {
		return
	}
	cmd := string(body[:semi])
	data := body[semi+1:]
	switch cmd {
	case "0", "1", "2":
		if t.OnTitle != nil {
			t.OnTitle(string(data))
		}
	}
}

// readEmu reads device responses from the SafeEmulator and writes them back to the PTY.
func (t *Terminal) readEmu() {
	buf := make([]byte, 4096)
	for {
		select {
		case <-t.done:
			return
		default:
		}

		n, err := t.Emu.Read(buf)
		if n > 0 {
			_, _ = t.ptmx.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// GetCWD returns the current working directory of the shell process.
// Implementation is platform-specific (see terminal_linux.go / terminal_darwin.go).
func (t *Terminal) GetCWD() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cmd == nil || t.cmd.Process == nil {
		return ""
	}
	return processCWD(t.cmd.Process.Pid)
}

// ForegroundProcessName returns the executable name of the PTY's
// current foreground process group leader — what shell users would
// think of as "the thing currently running in this tab" (vim, top,
// ssh, etc). iTerm2 and most modern terminals use this to populate
// the window title when the app doesn't emit OSC 0/2 itself.
//
// We get the foreground process group ID via TIOCGPGRP on the PTY
// master fd, then look up that PID's name via the platform-specific
// processName (ps on macOS, /proc/<pid>/comm on Linux).
//
// Returns "" if anything in the lookup fails — caller should fall
// back to OSC title or a default.
func (t *Terminal) ForegroundProcessName() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ptmx == nil {
		return ""
	}
	pgid, err := unix.IoctlGetInt(int(t.ptmx.Fd()), unix.TIOCGPGRP)
	if err != nil || pgid <= 0 {
		return ""
	}
	return processName(pgid)
}

// Paste sends text to the PTY, wrapping with bracketed-paste markers
// (ESC[200~ ... ESC[201~) when the application has enabled DECSET 2004.
// Use this for all clipboard paste paths so editors/shells that opted
// into bracketed paste cannot interpret pasted newlines as commands.
func (t *Terminal) Paste(text string) {
	if t.bracketedPaste.Load() {
		_, _ = t.ptmx.WriteString("\x1b[200~" + text + "\x1b[201~")
		return
	}
	_, _ = t.ptmx.WriteString(text)
}

// scrollbackCell holds serialized cell data for persistence.
type scrollbackCell struct {
	Content string
}

// SaveScrollback serializes the current scrollback buffer to a file.
func (t *Terminal) SaveScrollback(filePath string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	sbLen := t.Emu.ScrollbackLen()
	cols := t.Emu.Width()
	lines := make([][]scrollbackCell, sbLen)

	for row := 0; row < sbLen; row++ {
		line := make([]scrollbackCell, cols)
		for col := 0; col < cols; col++ {
			cell := t.Emu.ScrollbackCellAt(col, row)
			if cell != nil {
				line[col] = scrollbackCell{
					Content: cell.Content,
				}
			}
		}
		lines[row] = line
	}

	data, err := json.Marshal(lines)
	if err != nil {
		return err
	}

	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	return os.WriteFile(filePath, data, 0600)
}

// LoadScrollback deserializes a scrollback buffer from a file.
// Note: This loads the data but does not restore it to the emulator
// (charmbracelet/x/vt does not provide an API to modify scrollback after creation).
// This is preserved for future use or alternative implementations.
func (t *Terminal) LoadScrollback(filePath string) ([][]scrollbackCell, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var lines [][]scrollbackCell
	if err := json.Unmarshal(data, &lines); err != nil {
		return nil, err
	}

	return lines, nil
}
