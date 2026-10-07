// Package supervise keeps a daemon's process plumbing alive across
// the daemon's own death. `xerotty serve` runs as a small supervisor
// that owns the wire listener, holds duplicates of every tab's PTY
// master and scrollback file, and restarts the daemon child from a
// hot-upgrade-style handoff when it dies. See docs/CRASH_RESTORE_PLAN.md.
//
// This file is the control channel between the two: a socketpair the
// child inherits, carrying length-prefixed frames. File descriptors
// travel as SCM_RIGHTS on the frame that introduces them.
package supervise

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
)

// Frame kinds.
const (
	KindTab     byte = 1 // child → sup: TabMsg + fds [ptmx, disk?]
	KindTabGone byte = 2 // child → sup: TabGoneMsg
	KindState   byte = 3 // child → sup: raw msgpack handoff.State (no fds/screens)
	KindExit    byte = 4 // sup → child: ExitMsg
	KindUpgrade byte = 5 // sup → child: UpgradeMsg
	KindCaps    byte = 6 // sup → child: CapsMsg, once right after spawn
)

// ExitUpgrade is the daemon child's exit status after it wrote the
// handoff an UpgradeMsg asked for: the supervisor resumes the next
// child from that handoff instead of treating the exit as a crash.
// 75 is EX_TEMPFAIL from sysexits.h; nothing else in xerotty uses it.
const ExitUpgrade = 75

// TabMsg introduces a tab's process plumbing. The frame carries the
// PTY master as fds[0] and, when present, the disk scrollback file as
// fds[1].
type TabMsg struct {
	ID  uint32 `json:"id"`
	PID int    `json:"pid"`
}

// TabGoneMsg retires a tab: the supervisor closes its copies.
type TabGoneMsg struct {
	ID uint32 `json:"id"`
}

// ExitMsg reports a shell exit the daemon cannot observe itself (a
// child it did not spawn — see "foreign children" in the plan).
type ExitMsg struct {
	PID  int `json:"pid"`
	Code int `json:"code"`
}

// UpgradeMsg asks the daemon child to stop serving, write its full
// session handoff (screens, modes, scrollback index) to Handoff and
// exit with ExitUpgrade. The supervisor already holds every tab's
// PTY master and scrollback file, so nothing else has to travel.
type UpgradeMsg struct {
	Handoff string `json:"handoff"`
}

// CapsMsg tells the child what its supervisor does. A supervisor
// that predates a field never sends it, so the zero value is the old
// behavior. Upgrade: the supervisor handles SIGUSR2 itself, so the
// child must not also exec-in-place on a SIGUSR2 that reached it
// (a pkill that hits both processes).
type CapsMsg struct {
	Upgrade bool `json:"upgrade"`
}

// maxFrame bounds a frame payload. State frames carry topology and
// names, not screens, so this is generous.
const maxFrame = 16 << 20

// Conn is one end of the control channel.
type Conn struct {
	uc  *net.UnixConn
	wmu sync.Mutex // one frame at a time: a short write is finished in a second call
}

// Pair creates the socketpair. The child end is meant for
// exec.Cmd.ExtraFiles; the parent end is wrapped immediately.
func Pair() (parent *Conn, child *os.File, err error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("supervise: socketpair: %w", err)
	}
	pf := os.NewFile(uintptr(fds[0]), "supervise-parent")
	cf := os.NewFile(uintptr(fds[1]), "supervise-child")
	c, err := Wrap(pf)
	if err != nil {
		pf.Close()
		cf.Close()
		return nil, nil, err
	}
	return c, cf, nil
}

// Wrap turns an inherited socket file into a Conn. FileConn dups the
// descriptor, so the caller's file is closed here.
func Wrap(f *os.File) (*Conn, error) {
	nc, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		return nil, fmt.Errorf("supervise: control fd: %w", err)
	}
	uc, ok := nc.(*net.UnixConn)
	if !ok {
		nc.Close()
		return nil, errors.New("supervise: control fd is not a unix socket")
	}
	return &Conn{uc: uc}, nil
}

// Close closes the channel. The other end reads EOF.
func (c *Conn) Close() error { return c.uc.Close() }

// Send writes one frame. fds, if any, are sent as SCM_RIGHTS attached
// to the frame's first byte; the receiver gets its own descriptors,
// so the caller may close its copies afterwards.
func (c *Conn) Send(kind byte, payload []byte, fds ...int) error {
	if len(payload) > maxFrame {
		return fmt.Errorf("supervise: frame of %d bytes exceeds limit", len(payload))
	}
	buf := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(len(payload)))
	buf[4] = kind
	copy(buf[5:], payload)
	var oob []byte
	if len(fds) > 0 {
		oob = syscall.UnixRights(fds...)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	// One sendmsg for the whole frame: ancillary data is delivered
	// with the first byte the receiver reads, and the receiver reads
	// the header first.
	n, _, err := c.uc.WriteMsgUnix(buf, oob, nil)
	if err != nil {
		return err
	}
	if n < len(buf) {
		// Stream socket: a short write of the data part is legal.
		// The oob went with the first chunk; finish the rest plainly.
		if _, err := c.uc.Write(buf[n:]); err != nil {
			return err
		}
	}
	return nil
}

// SendJSON marshals v as the payload of a frame.
func (c *Conn) SendJSON(kind byte, v any, fds ...int) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Send(kind, b, fds...)
}

// Recv reads one frame and any descriptors attached to it. The
// returned files are owned by the caller.
func (c *Conn) Recv() (kind byte, payload []byte, files []*os.File, err error) {
	hdr := make([]byte, 5)
	oob := make([]byte, 256)
	got := 0
	for got < len(hdr) {
		n, oobn, _, _, rerr := c.uc.ReadMsgUnix(hdr[got:], oob)
		if n > 0 {
			got += n
		}
		if oobn > 0 {
			fs, perr := parseRights(oob[:oobn])
			if perr != nil {
				closeAll(files)
				return 0, nil, nil, perr
			}
			files = append(files, fs...)
		}
		if rerr != nil {
			if got == 0 && errors.Is(rerr, io.EOF) {
				closeAll(files)
				return 0, nil, nil, io.EOF
			}
			if got < len(hdr) {
				closeAll(files)
				return 0, nil, nil, rerr
			}
		}
	}
	size := binary.BigEndian.Uint32(hdr)
	kind = hdr[4]
	if size > maxFrame {
		closeAll(files)
		return 0, nil, nil, fmt.Errorf("supervise: frame of %d bytes exceeds limit", size)
	}
	payload = make([]byte, size)
	if _, err := io.ReadFull(c.uc, payload); err != nil {
		closeAll(files)
		return 0, nil, nil, err
	}
	return kind, payload, files, nil
}

func parseRights(oob []byte) ([]*os.File, error) {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, fmt.Errorf("supervise: parse control message: %w", err)
	}
	var files []*os.File
	for _, m := range msgs {
		fds, err := syscall.ParseUnixRights(&m)
		if err != nil {
			closeAll(files)
			return nil, fmt.Errorf("supervise: parse rights: %w", err)
		}
		for _, fd := range fds {
			syscall.CloseOnExec(fd)
			files = append(files, os.NewFile(uintptr(fd), "supervise-rights"))
		}
	}
	return files, nil
}

func closeAll(files []*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}
