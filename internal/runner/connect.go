package runner

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/protocol"
)

// Connect runs the `xerotty connect` subcommand: a CLI thin client
// that attaches to a local or remote xerotty serve daemon and
// proxies the user's terminal to it. Useful for testing the
// daemon end-to-end, scripting against it, or just running a
// daemon-backed terminal from inside another terminal (e.g. tmux).
//
//	xerotty connect                            # local default socket
//	xerotty connect --socket /path/sock         # explicit socket
//	xerotty connect --ssh user@host             # spawn `ssh ... xerotty serve --stdio`
//
// Press Ctrl-q to quit the client (the daemon-side session keeps
// running).
func Connect(args []string) int {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	var socketPath string
	var sshDest string
	var sshArgs string
	var remoteCmd string
	fs.StringVar(&socketPath, "socket", "", "unix socket path of the daemon (default: $XDG_RUNTIME_DIR/xerottyd.sock)")
	fs.StringVar(&sshDest, "ssh", "", "spawn `ssh <dest> xerotty serve --stdio` instead of using a local socket")
	fs.StringVar(&sshArgs, "ssh-args", "", "extra args before the SSH destination (e.g. \"-i ~/.ssh/key -p 2222\")")
	fs.StringVar(&remoteCmd, "remote-cmd", "", "remote command for --ssh mode (default: \"xerotty serve --stdio\")")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	oldState, err := makeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "xerotty connect: raw mode: %v\n", err)
		return 1
	}
	defer restoreTerm(int(os.Stdin.Fd()), oldState)

	var c *clientproto.Client
	if sshDest != "" {
		if remoteCmd == "" {
			remoteCmd = "xerotty serve --stdio"
		}
		extra := strings.Fields(sshArgs)
		c, err = clientproto.DialSSH(sshDest, remoteCmd, extra)
		if err != nil {
			fmt.Fprintf(os.Stderr, "xerotty connect: dial ssh %s: %v\n", sshDest, err)
			return 1
		}
	} else {
		if socketPath == "" {
			socketPath = defaultSocketPath()
		}
		c, err = clientproto.Dial(socketPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "xerotty connect: dial %s: %v\n", socketPath, err)
			return 1
		}
	}
	defer c.Close()

	if _, err := c.Hello("xerotty-connect"); err != nil {
		fmt.Fprintf(os.Stderr, "xerotty connect: hello: %v\n", err)
		return 1
	}
	go c.Run()

	if err := c.Attach("", true); err != nil {
		fmt.Fprintf(os.Stderr, "xerotty connect: attach: %v\n", err)
		return 1
	}

	v := &connectClient{c: c}
	if err := v.run(); err != nil {
		fmt.Fprintf(os.Stderr, "xerotty connect: %v\n", err)
		return 1
	}
	return 0
}

type connectClient struct {
	c *clientproto.Client

	mu     sync.Mutex
	tabID  uint32
	cols   uint16
	rows   uint16
	curRow uint16
	curCol uint16

	// Local mirror of the daemon's cell grid: CellFull replaces
	// wholesale, CellDiff patches individual cells. Repaint after
	// either.
	mirror [][]protocol.Cell
}

func (v *connectClient) run() error {
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

	rows, cols := terminalSize()
	if cols > 0 && rows > 0 {
		_ = v.c.SendResize(v.tabID, uint16(cols), uint16(rows))
	}
	fmt.Fprint(os.Stdout, "\x1b[2J\x1b[H")

	go v.pumpStdin()
	go v.pumpResize()

	for {
		select {
		case full := <-v.c.CellFull():
			v.applyFull(full)
			v.repaint()
		case diff := <-v.c.CellDiff():
			v.applyDiff(diff)
			v.repaint()
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

// pumpStdin forwards stdin bytes to the daemon. Ctrl-q (0x11) quits
// the client locally without killing the daemon-side session.
func (v *connectClient) pumpStdin() {
	r := bufio.NewReader(os.Stdin)
	buf := make([]byte, 256)
	for {
		n, err := r.Read(buf)
		if err != nil {
			return
		}
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

func (v *connectClient) pumpResize() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	for range ch {
		rows, cols := terminalSize()
		if cols == 0 || rows == 0 {
			continue
		}
		_ = v.c.SendResize(v.tabID, uint16(cols), uint16(rows))
		fmt.Fprint(os.Stdout, "\x1b[2J\x1b[H")
	}
}

func (v *connectClient) applyFull(f *protocol.CellFull) {
	if f.ID != v.tabID {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.cols = f.Cols
	v.rows = f.Rows
	v.mirror = f.Grid
}

func (v *connectClient) applyDiff(d *protocol.CellDiff) {
	if d.ID != v.tabID {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, e := range d.Cells {
		if int(e.Row) >= len(v.mirror) {
			continue
		}
		if int(e.Col) >= len(v.mirror[e.Row]) {
			continue
		}
		v.mirror[e.Row][e.Col] = e.Cell
	}
}

func (v *connectClient) repaint() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.mirror == nil {
		return
	}
	var sb strings.Builder
	sb.WriteString("\x1b[H")
	for r, row := range v.mirror {
		sb.WriteString(fmt.Sprintf("\x1b[%d;1H", r+1))
		for _, cell := range row {
			sb.WriteString(sgrForStyle(cell.Style, cell.FgRGB, cell.BgRGB))
			ch := cell.Content
			if ch == "" {
				ch = " "
			}
			sb.WriteString(ch)
		}
		sb.WriteString("\x1b[0m")
	}
	sb.WriteString(fmt.Sprintf("\x1b[%d;%dH", v.curRow+1, v.curCol+1))
	_, _ = os.Stdout.WriteString(sb.String())
}

func (v *connectClient) moveCursor(row, col uint16) {
	fmt.Fprintf(os.Stdout, "\x1b[%d;%dH", row+1, col+1)
}

// sgrForStyle translates a packed protocol.Style into the SGR escape
// sequence that produces the same visual on a host terminal that
// understands xterm-256color + (where supported) 24-bit truecolor.
func sgrForStyle(style uint32, fgRGB, bgRGB uint32) string {
	attrs, _, fgIdx, fgIsRGB, bgIdx, bgIsRGB := protocol.UnpackStyle(style)
	parts := []string{"0"}
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
	fmt.Fprint(os.Stdout, "\x1b[?25h\x1b[0m\r\n")
}

func terminalSize() (int, int) {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0
	}
	return int(ws.Row), int(ws.Col)
}
