package clientproto_test

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
)

// wedgedConn is a net.Conn that never makes progress: Read and Write
// both block until Close, and SetWriteDeadline is a NO-OP (mirroring
// SSH-stdio, where the write deadline can't catch a stall). It models a
// hung daemon whose receive buffer has filled — our writes wedge and no
// inbound frame (hence no pong) ever arrives. The ONLY detector here is
// the heartbeat's dual-clock, which is exactly what the test exercises.
type wedgedConn struct {
	closed chan struct{}
	once   sync.Once
}

func newWedgedConn() *wedgedConn { return &wedgedConn{closed: make(chan struct{})} }

func (c *wedgedConn) Read(b []byte) (int, error)  { <-c.closed; return 0, io.EOF }
func (c *wedgedConn) Write(b []byte) (int, error) { <-c.closed; return 0, io.ErrClosedPipe }
func (c *wedgedConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}
func (c *wedgedConn) LocalAddr() net.Addr                { return wedgedAddr{} }
func (c *wedgedConn) RemoteAddr() net.Addr               { return wedgedAddr{} }
func (c *wedgedConn) SetDeadline(t time.Time) error      { return nil }
func (c *wedgedConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *wedgedConn) SetWriteDeadline(t time.Time) error { return nil }

type wedgedAddr struct{}

func (wedgedAddr) Network() string { return "wedged" }
func (wedgedAddr) String() string  { return "wedged" }

// TestClientHeartbeatReapsWedgedDaemon verifies a reap when writes block
// outright (full send buffer / dead link) and nothing comes back: no
// inbound + no pong → reap. Complementary to the hung-but-writable case
// below — these are the two distinct dead-peer failure modes.
func TestClientHeartbeatReapsWedgedDaemon(t *testing.T) {
	conn := newWedgedConn()
	c := clientproto.Wrap(conn)
	c.SetHeartbeat(20*time.Millisecond, 120*time.Millisecond)

	go c.Run()

	select {
	case <-c.Closed():
		// Reaped — good.
	case <-time.After(3 * time.Second):
		t.Fatalf("client did not reap a wedged daemon within the liveness window")
	}
	_ = conn.Close()
}

// hungWritableConn models the SIGSTOP'd / deadlocked-but-socket-open
// daemon that the live test caught: WRITES SUCCEED (the kernel keeps
// buffering our tiny pings — the buffer never fills at heartbeat volume),
// but NO inbound ever arrives and NO pong is returned. The old reaper
// keyed on write-progress, which these succeeding ping-writes refreshed
// forever, so it NEVER fired. The fix keys on inbound+pong, both of which
// stay stale here.
type hungWritableConn struct {
	closed chan struct{}
	once   sync.Once
}

func newHungWritableConn() *hungWritableConn {
	return &hungWritableConn{closed: make(chan struct{})}
}

func (c *hungWritableConn) Read(b []byte) (int, error)  { <-c.closed; return 0, io.EOF }
func (c *hungWritableConn) Write(b []byte) (int, error) { return len(b), nil } // accept + discard
func (c *hungWritableConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}
func (c *hungWritableConn) LocalAddr() net.Addr                { return wedgedAddr{} }
func (c *hungWritableConn) RemoteAddr() net.Addr               { return wedgedAddr{} }
func (c *hungWritableConn) SetDeadline(t time.Time) error      { return nil }
func (c *hungWritableConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *hungWritableConn) SetWriteDeadline(t time.Time) error { return nil }

// TestClientHeartbeatReapsHungWritableDaemon is the regression for the
// live-found bug: a hung-but-socket-open daemon whose buffer still
// accepts our pings must still be reaped within ~window. Fails on the
// old write-progress reaper (succeeding pings kept it asleep forever);
// passes on the inbound+pong reaper.
func TestClientHeartbeatReapsHungWritableDaemon(t *testing.T) {
	conn := newHungWritableConn()
	c := clientproto.Wrap(conn)
	c.SetHeartbeat(20*time.Millisecond, 120*time.Millisecond)

	go c.Run()

	select {
	case <-c.Closed():
		// Reaped despite writes succeeding — the actual fix.
	case <-time.After(3 * time.Second):
		t.Fatalf("client did not reap a hung-but-writable daemon — our own pings kept the reaper asleep")
	}
	_ = conn.Close()
}

// TestClientHeartbeatKeepsLiveConn verifies the converse: a healthy
// daemon — one that drains our writes (write-progress fresh) AND answers
// our pings with Pongs (pong clock fresh) — is never reaped, across
// several windows. Both dual-clock signals stay fresh.
func TestClientHeartbeatKeepsLiveConn(t *testing.T) {
	cliConn, peerConn := net.Pipe()
	c := clientproto.Wrap(cliConn)
	c.SetHeartbeat(30*time.Millisecond, 200*time.Millisecond)

	stop := make(chan struct{})
	defer close(stop)
	// Peer: drain client writes (keeps write-progress fresh) AND
	// periodically send a Pong (keeps the pong clock fresh).
	go func() {
		buf := make([]byte, 4096)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = peerConn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
			_, _ = peerConn.Read(buf)
		}
	}()
	go func() {
		tick := time.NewTicker(40 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				_ = peerConn.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
				// Frame: [u32 len BE=1][u8 MsgPong]. A zero-body pong.
				_, _ = peerConn.Write([]byte{0, 0, 0, 1, byte(44)})
			}
		}
	}()

	go c.Run()

	select {
	case <-c.Closed():
		t.Fatalf("client reaped a live (frame-emitting) daemon")
	case <-time.After(700 * time.Millisecond):
		// Survived several windows while frames flowed — correct.
	}
}
