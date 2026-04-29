package terminal

import (
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
)

// Terminal wraps a SafeEmulator + PTY + reader goroutines for one tab.
type Terminal struct {
	Emu      *vt.SafeEmulator
	ptmx     *os.File
	cmd      *exec.Cmd
	DataCh   chan struct{} // signals new data for rendering (buffered, cap 1)
	OnTitle  func(string)  // called when OSC 0/2 sets window title
	cols     int
	rows     int
	closed   bool
	ExitCode int  // exit code of child process (-1 = still running or unknown)
	mu       sync.Mutex
	done     chan struct{}
}

// New creates a terminal with the given dimensions and starts the shell.
func New(cfg *config.Config, cols, rows int) (*Terminal, error) {
	ptmx, cmd, err := spawnPTY(cfg, uint16(cols), uint16(rows))
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

	emu.Emulator.SetCallbacks(vt.Callbacks{
		Title: func(title string) {
			if t.OnTitle != nil {
				t.OnTitle(title)
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

// Close shuts down the terminal: kills the child, closes the PTY, stops goroutines.
func (t *Terminal) Close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.mu.Unlock()

	close(t.done)
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	_ = t.ptmx.Close()
	// waitChild goroutine handles cmd.Wait()
}

// IsClosed returns true if the terminal has been closed.
func (t *Terminal) IsClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// waitChild waits for the child process to exit and records its exit code.
func (t *Terminal) waitChild() {
	err := t.cmd.Wait()
	t.mu.Lock()
	defer t.mu.Unlock()
	if err == nil {
		t.ExitCode = 0
	} else if exitErr, ok := err.(*exec.ExitError); ok {
		t.ExitCode = exitErr.ExitCode()
	} else {
		t.ExitCode = 1
	}
	t.closed = true
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
			}
			select {
			case t.DataCh <- struct{}{}:
			default:
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
