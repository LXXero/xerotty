package protocol

import (
	"io"
	"net"
	"time"
)

// StdioConn adapts a separate Reader + WriteCloser pair (typically
// os.Stdin + os.Stdout, or an exec.Cmd's stdout + stdin pipes) into
// a net.Conn so the daemon and client code paths can stay socket-
// generic.
//
// Deadlines + addresses are no-ops since they don't apply to a
// piped transport. Use stderr separately for logs — the protocol
// owns stdin/stdout exclusively.
type StdioConn struct {
	r io.Reader
	w io.WriteCloser
}

// NewStdioConn wraps r (read side) + w (write side) as a net.Conn.
func NewStdioConn(r io.Reader, w io.WriteCloser) *StdioConn {
	return &StdioConn{r: r, w: w}
}

// Read reads from the read side.
func (c *StdioConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// Write writes to the write side.
func (c *StdioConn) Write(b []byte) (int, error) { return c.w.Write(b) }

// Close closes the write side. The read side (typically stdin) is
// closed by the OS when our parent process exits.
func (c *StdioConn) Close() error { return c.w.Close() }

// LocalAddr / RemoteAddr return a fake stdio addr so callers that
// log the conn don't panic.
func (c *StdioConn) LocalAddr() net.Addr  { return stdioAddr{} }
func (c *StdioConn) RemoteAddr() net.Addr { return stdioAddr{} }

// Deadlines aren't supported on pipes. Return nil (no error) to
// stay compatible with net.Conn — callers that need deadlines
// will get blocking reads / writes, which is what stdio does
// natively anyway.
func (c *StdioConn) SetDeadline(t time.Time) error      { return nil }
func (c *StdioConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *StdioConn) SetWriteDeadline(t time.Time) error { return nil }

type stdioAddr struct{}

func (stdioAddr) Network() string { return "stdio" }
func (stdioAddr) String() string  { return "stdio" }
