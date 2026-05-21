// xerotty-viewer is a thin terminal-based viewer for an xerottyd
// session. Connects to a daemon, attaches to the default session,
// and proxies stdin keystrokes ↔ tab PTY ↔ rendered cell grid on
// stdout. Useful for testing the daemon end-to-end without needing
// the full xerotty GUI; also the cleanest concrete demonstration of
// what a fully-thin xerotty UI client looks like protocol-wise.
//
// Renders each CellFull frame by repainting the visible grid via
// ANSI cursor-position escapes. No glyph cache, no font handling,
// no themes — your host terminal's font + theme are what you see.
// Cell.Style attributes get translated to SGR escape sequences
// (bold, italic, fg/bg) so colored / bold output from the daemon
// shell still looks colored / bold in your local terminal.
//
// Usage:
//
//	xerotty-viewer                        # connect to default socket
//	xerotty-viewer --socket /path/sock     # explicit socket
//
// Default socket path mirrors xerottyd's: $XDG_RUNTIME_DIR/
// xerottyd.sock, falling back to /tmp/xerottyd-$UID.sock.
//
// Press Ctrl-q to quit (the viewer; the daemon-side session keeps
// running). Press Ctrl-\ for SIGQUIT if Ctrl-q gets eaten.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/protocol"
	"golang.org/x/sys/unix"
)

func main() {
	var socketPath string
	flag.StringVar(&socketPath, "socket", "", "unix socket path of the daemon (default: $XDG_RUNTIME_DIR/xerottyd.sock)")
	flag.Parse()

	if socketPath == "" {
		socketPath = defaultSocketPath()
	}

	// Switch stdin into raw mode so keystrokes hit us byte-by-byte
	// without the line discipline cooking them. Restore on exit.
	oldState, err := makeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "xerotty-viewer: raw mode: %v\n", err)
		os.Exit(1)
	}
	defer restoreTerm(int(os.Stdin.Fd()), oldState)

	c, err := clientproto.Dial(socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "xerotty-viewer: dial %s: %v\n", socketPath, err)
		os.Exit(1)
	}
	defer c.Close()

	if _, err := c.Hello("xerotty-viewer"); err != nil {
		fmt.Fprintf(os.Stderr, "xerotty-viewer: hello: %v\n", err)
		os.Exit(1)
	}

	go c.Run()

	if err := c.Attach("", true); err != nil {
		fmt.Fprintf(os.Stderr, "xerotty-viewer: attach: %v\n", err)
		os.Exit(1)
	}

	v := &viewer{c: c}
	if err := v.run(); err != nil {
		fmt.Fprintf(os.Stderr, "xerotty-viewer: %v\n", err)
	}
}

type viewer struct {
	c *clientproto.Client

	mu       sync.Mutex
	tabID    uint32
	cols     uint16
	rows     uint16
	curRow   uint16
	curCol   uint16
}

func (v *viewer) run() error {
	// Block until Attached arrives so we know the first tab ID.
	attached := <-v.c.Attached()
	if len(attached.Tabs) == 0 {
		return fmt.Errorf("daemon attached with zero tabs")
	}
	t := attached.Tabs[0]
	v.mu.Lock()
	v.tabID = t.ID
	v.cols = t.Cols
	v.rows = t.Rows
	v.mu.Unlock()

	// Tell the daemon what cell dimensions we plan to render at.
	rows, cols := terminalSize()
	if cols > 0 && rows > 0 {
		_ = v.c.SendResize(v.tabID, uint16(cols), uint16(rows))
	}

	// Clear screen + park cursor so first paint is clean.
	fmt.Fprint(os.Stdout, "\x1b[2J\x1b[H")

	// stdin → InputBytes pump
	go v.pumpStdin()
	// SIGWINCH → Resize pump
	go v.pumpResize()

	// Main event loop: dispatch incoming frames.
	for {
		select {
		case full := <-v.c.CellFull():
			v.renderFull(full)
		case cur := <-v.c.Cursor():
			v.mu.Lock()
			v.curRow = cur.Row
			v.curCol = cur.Col
			v.mu.Unlock()
			v.moveCursor(cur.Row, cur.Col)
		case <-v.c.Closed():
			fmt.Fprint(os.Stdout, "\r\n[disconnected]\r\n")
			return v.c.ExitErr()
		}
	}
}

// pumpStdin reads stdin in raw mode and forwards bytes to the daemon
// tab. Ctrl-q (0x11) quits the viewer locally without killing the
// daemon-side session.
func (v *viewer) pumpStdin() {
	r := bufio.NewReader(os.Stdin)
	buf := make([]byte, 256)
	for {
		n, err := r.Read(buf)
		if err != nil {
			return
		}
		// Scan for Ctrl-q (DC1) — viewer quit hotkey. Forward the
		// preceding bytes if any.
		for i := 0; i < n; i++ {
			if buf[i] == 0x11 {
				if i > 0 {
					_ = v.c.SendInput(v.tabID, buf[:i])
				}
				_ = v.c.Detach()
				v.c.Close()
				return
			}
		}
		_ = v.c.SendInput(v.tabID, buf[:n])
	}
}

func (v *viewer) pumpResize() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	for range ch {
		rows, cols := terminalSize()
		if cols == 0 || rows == 0 {
			continue
		}
		_ = v.c.SendResize(v.tabID, uint16(cols), uint16(rows))
		// Force a repaint by clearing — daemon will ship a fresh
		// CellFull within a frame or two.
		fmt.Fprint(os.Stdout, "\x1b[2J\x1b[H")
	}
}

func (v *viewer) renderFull(f *protocol.CellFull) {
	if f.ID != v.tabID {
		return // ignore frames for tabs we're not viewing
	}
	v.mu.Lock()
	v.cols = f.Cols
	v.rows = f.Rows
	v.mu.Unlock()

	var sb strings.Builder
	// Park cursor at home before redrawing.
	sb.WriteString("\x1b[H")
	for r, row := range f.Grid {
		// Move to start of this row.
		sb.WriteString(fmt.Sprintf("\x1b[%d;1H", r+1))
		for _, cell := range row {
			sb.WriteString(sgrForStyle(cell.Style, cell.FgRGB, cell.BgRGB))
			c := cell.Content
			if c == "" {
				c = " "
			}
			sb.WriteString(c)
		}
		// Reset SGR at end of row so cursor restore doesn't carry
		// state into following stdout writes.
		sb.WriteString("\x1b[0m")
	}
	// Restore cursor to the daemon's reported position.
	v.mu.Lock()
	sb.WriteString(fmt.Sprintf("\x1b[%d;%dH", v.curRow+1, v.curCol+1))
	v.mu.Unlock()
	_, _ = os.Stdout.WriteString(sb.String())
}

func (v *viewer) moveCursor(row, col uint16) {
	fmt.Fprintf(os.Stdout, "\x1b[%d;%dH", row+1, col+1)
}

// sgrForStyle translates a packed protocol.Style into the SGR escape
// sequence that produces the same visual on a host terminal that
// understands xterm-256color + (where supported) 24-bit truecolor.
func sgrForStyle(style uint32, fgRGB, bgRGB uint32) string {
	attrs, _, fgIdx, fgIsRGB, bgIdx, bgIsRGB := protocol.UnpackStyle(style)
	parts := []string{"0"} // start fresh — viewer never assumes prior SGR
	if attrs&protocol.AttrBold != 0 {
		parts = append(parts, "1")
	}
	if attrs&protocol.AttrFaint != 0 {
		parts = append(parts, "2")
	}
	if attrs&protocol.AttrItalic != 0 {
		parts = append(parts, "3")
	}
	if attrs&protocol.AttrReverse != 0 {
		parts = append(parts, "7")
	}
	if attrs&protocol.AttrConceal != 0 {
		parts = append(parts, "8")
	}
	if attrs&protocol.AttrStrike != 0 {
		parts = append(parts, "9")
	}
	if attrs&protocol.AttrBlink != 0 {
		parts = append(parts, "5")
	}
	if fgIsRGB {
		r, g, b := uint8(fgRGB>>16), uint8(fgRGB>>8), uint8(fgRGB)
		parts = append(parts, "38;2;"+strconv.Itoa(int(r))+";"+strconv.Itoa(int(g))+";"+strconv.Itoa(int(b)))
	} else if fgIdx != 0 {
		parts = append(parts, "38;5;"+strconv.Itoa(int(fgIdx)))
	}
	if bgIsRGB {
		r, g, b := uint8(bgRGB>>16), uint8(bgRGB>>8), uint8(bgRGB)
		parts = append(parts, "48;2;"+strconv.Itoa(int(r))+";"+strconv.Itoa(int(g))+";"+strconv.Itoa(int(b)))
	} else if bgIdx != 0 {
		parts = append(parts, "48;5;"+strconv.Itoa(int(bgIdx)))
	}
	return "\x1b[" + strings.Join(parts, ";") + "m"
}

// makeRaw / restoreTerm flip stdin into raw mode so we get
// byte-by-byte keystrokes instead of line-buffered cooked input.
func makeRaw(fd int) (*unix.Termios, error) {
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}
	raw := *old
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return nil, err
	}
	return old, nil
}

func restoreTerm(fd int, t *unix.Termios) {
	if t == nil {
		return
	}
	_ = unix.IoctlSetTermios(fd, unix.TCSETS, t)
	fmt.Fprint(os.Stdout, "\x1b[?25h\x1b[0m\r\n") // show cursor + reset SGR
}

// terminalSize returns rows, cols of the host terminal.
func terminalSize() (int, int) {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0
	}
	return int(ws.Row), int(ws.Col)
}

// defaultSocketPath mirrors xerottyd's logic so they agree without
// hardcoding the path in two places.
func defaultSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "xerottyd.sock")
	}
	return filepath.Join(os.TempDir(), "xerottyd-"+strconv.Itoa(os.Getuid())+".sock")
}
