package clientproto_test

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/protocol"
)

// writeFrame hand-frames one message onto w: [u32 len BE][u8 type][body].
func writeFrame(t *testing.T, w io.Writer, typ protocol.MsgType, msg interface {
	MarshalMsg([]byte) ([]byte, error)
}) {
	t.Helper()
	body, err := msg.MarshalMsg(nil)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var header [5]byte
	binary.BigEndian.PutUint32(header[:4], uint32(1+len(body)))
	header[4] = byte(typ)
	if _, err := w.Write(header[:]); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatalf("write body: %v", err)
	}
}

// TestPongsFlowWhileConsumerStalled is the reap-storm regression: with
// NOBODY draining the payload channels (a stalled GUI), the daemon
// floods payload frames and then pings. The client must keep reading
// the socket and answer the ping — before the read-loop/dispatch split,
// the 17th undrained Bell wedged the read loop mid-send, the ping was
// never read, no pong went back, and the daemon reaped a live client
// (xerottyd.log wall-to-wall with "reaping unresponsive client").
func TestPongsFlowWhileConsumerStalled(t *testing.T) {
	cliConn, peerConn := net.Pipe()
	defer cliConn.Close()
	defer peerConn.Close()

	c := clientproto.Wrap(cliConn)
	// Long window: this test is about pong responsiveness, not reaping.
	c.SetHeartbeat(10*time.Second, time.Minute)
	go c.Run()

	// Peer reader: drain everything the client writes, watching for the
	// pong that echoes our nonce.
	const wantNonce = 7
	gotPong := make(chan struct{})
	go func() {
		r := protocol.NewFrameReader(peerConn)
		for {
			typ, body, err := r.ReadFrame()
			if err != nil {
				return
			}
			if typ != protocol.MsgPong {
				continue
			}
			pong := &protocol.Pong{}
			if _, err := pong.UnmarshalMsg(body); err != nil {
				continue
			}
			if pong.Nonce == wantNonce {
				close(gotPong)
				return
			}
		}
	}()

	// Flood far more payload frames than every channel + buffer could
	// hold. net.Pipe is synchronous: each Write completes only when the
	// client's read loop actually reads it, so a wedged read loop makes
	// this flood (and the ping after it) hang — which is the regression.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 300; i++ {
			writeFrame(t, peerConn, protocol.MsgBell, &protocol.Bell{ID: 1})
		}
		writeFrame(t, peerConn, protocol.MsgPing, &protocol.Ping{Nonce: wantNonce})
	}()

	select {
	case <-gotPong:
		// Pong arrived while 300 undrained frames sat queued — the read
		// loop stayed live. The actual fix.
	case <-time.After(3 * time.Second):
		t.Fatalf("no pong within 3s while payload consumers were stalled — read loop wedged")
	}
	<-done
}
