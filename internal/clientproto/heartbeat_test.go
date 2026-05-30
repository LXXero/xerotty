package clientproto_test

import (
	"net"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
)

// TestClientHeartbeatReapsSilentDaemon verifies the layer-4d client
// heartbeat: when the daemon stops sending ANYTHING (hung / dead SSH),
// the client tears its own connection down within the liveness window so
// the Hub can re-dial. The peer here never replies, so lastInbound stays
// at connect time and the window trips.
func TestClientHeartbeatReapsSilentDaemon(t *testing.T) {
	cliConn, peerConn := net.Pipe()
	c := clientproto.Wrap(cliConn)
	c.SetHeartbeat(30*time.Millisecond, 150*time.Millisecond)

	// Drain the peer side so the client's writer (Hello-less here, but it
	// emits heartbeat pings) never blocks on the synchronous pipe. Never
	// write back — that's the "silent daemon" the heartbeat must catch.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := peerConn.Read(buf); err != nil {
				return
			}
		}
	}()

	go c.Run()

	select {
	case <-c.Closed():
		// Reaped — good.
	case <-time.After(3 * time.Second):
		t.Fatalf("client did not reap a silent daemon within the liveness window")
	}
	_ = peerConn.Close()
}

// TestClientHeartbeatKeepsLiveConn verifies the converse: a peer that
// keeps sending frames (here, raw bytes that bump lastInbound on every
// ReadFrame... actually we must send VALID frames) keeps the conn alive
// past several windows. We send periodic Pong frames as the daemon's
// keep-alive analogue.
func TestClientHeartbeatKeepsLiveConn(t *testing.T) {
	cliConn, peerConn := net.Pipe()
	c := clientproto.Wrap(cliConn)
	c.SetHeartbeat(30*time.Millisecond, 200*time.Millisecond)

	stop := make(chan struct{})
	defer close(stop)
	// Peer: drain client writes AND periodically send a Pong so the
	// client's lastInbound stays fresh.
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
