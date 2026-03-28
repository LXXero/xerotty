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
	Emu     *vt.SafeEmulator
	ptmx    *os.File
	cmd     *exec.Cmd
	DataCh  chan struct{} // signals new data for rendering (buffered, cap 1)
	OnTitle func(string)  // called when OSC 0/2 sets window title
	cols    int
	rows    int
	closed  bool
	mu      sync.Mutex
	done    chan struct{}
}

// New creates a terminal with the given dimensions and starts the shell.
func New(cfg *config.Config, cols, rows int) (*Terminal, error) {
	ptmx, cmd, err := spawnPTY(cfg, uint16(cols), uint16(rows))
	if err != nil {
		return nil, err
	}

	emu := vt.NewSafeEmulator(cols, rows)

	t := &Terminal{
		Emu:    emu,
		ptmx:   ptmx,
		cmd:    cmd,
		DataCh: make(chan struct{}, 1),
		cols:   cols,
		rows:   rows,
		done:   make(chan struct{}),
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
	_ = t.cmd.Wait()
}

// IsClosed returns true if the terminal has been closed.
func (t *Terminal) IsClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// readPTY reads from the PTY and writes to the SafeEmulator.
func (t *Terminal) readPTY() {
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-t.done:
			return
		default:
		}

		n, err := t.ptmx.Read(buf)
		if n > 0 {
			t.Emu.Write(buf[:n])
			// Non-blocking notify
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
