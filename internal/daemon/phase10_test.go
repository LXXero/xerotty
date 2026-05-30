package daemon_test

import (
	"net"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/protocol"
)

// readFrameExpect reads one frame and asserts its type.
func readFrameExpect(t *testing.T, fr *protocol.FrameReader, want protocol.MsgType) {
	t.Helper()
	got, _, err := fr.ReadFrame()
	if err != nil {
		t.Fatalf("read frame (want %v): %v", want, err)
	}
	if got != want {
		t.Fatalf("frame type = %v, want %v", got, want)
	}
}

// TestStuckClientDoesNotBlockOthers verifies Phase 10 layer 1: a client
// whose socket has wedged (never reads) must NOT block the daemon's
// mutating path or delivery to OTHER clients. B is served over an
// unbuffered net.Pipe and stops reading after attach, so the daemon's
// writer to B hard-blocks on the very next frame. With the synchronous
// broadcast (pre-layer-1), A's create would wedge in broadcastTopology
// writing to B; with the async per-client writers it sails through.
func TestStuckClientDoesNotBlockOthers(t *testing.T) {
	d, sockPath := startDaemon(t)

	// A: a normal client over the socket. Drain everything except the
	// channels we assert on (TabCreated + Topology).
	a := mustDial(t, sockPath, "client-A")
	defer a.Close()
	<-a.Attached()
	go func() {
		for {
			select {
			case <-a.Closed():
				return
			case <-a.CellFull():
			case <-a.CellDiff():
			case <-a.Cursor():
			case <-a.Title():
			case <-a.Bell():
			case <-a.ChildExit():
			case <-a.TabState():
			case <-a.ScrollbackAppend():
			case <-a.ScrollbackCleared():
			case <-a.WindowCreated():
			case <-a.ClipboardSet():
			case <-a.ProposalsChanged():
			case <-a.Errors():
			}
		}
	}()

	// B: a deliberately-stuck client. Served over an unbuffered pipe; we
	// do the handshake + attach by hand, then STOP reading, so the
	// daemon's writer to B blocks on the next frame.
	bClient, bServer := net.Pipe()
	go d.ServeConn(bServer)
	defer bClient.Close()
	bfr := protocol.NewFrameReader(bClient)
	if err := protocol.WriteFrame(bClient, protocol.MsgHello, &protocol.Hello{
		Version: protocol.ProtocolVersion, ClientID: "B-stuck",
	}); err != nil {
		t.Fatalf("B hello: %v", err)
	}
	readFrameExpect(t, bfr, protocol.MsgHelloAck)
	if err := protocol.WriteFrame(bClient, protocol.MsgAttach, &protocol.Attach{}); err != nil {
		t.Fatalf("B attach: %v", err)
	}
	readFrameExpect(t, bfr, protocol.MsgAttached)
	// From here B reads nothing — its server-side writer wedges.

	// Let B's publish loops start and its writer block on the pipe.
	time.Sleep(200 * time.Millisecond)

	// A mutates (create tab → broadcastTopology fans out to A AND the
	// stuck B) and must get its ack + topology promptly.
	if err := a.SendTabCreate(0, 80, 24, "", ""); err != nil {
		t.Fatalf("A create: %v", err)
	}
	select {
	case <-a.TabCreated():
	case <-time.After(5 * time.Second):
		t.Fatal("A's TabCreated never arrived — a stuck client blocked the daemon's mutating path")
	}
	select {
	case <-a.Topology():
	case <-time.After(5 * time.Second):
		t.Fatal("A's topology broadcast never arrived — stuck client blocked delivery to others")
	}

	// And A keeps getting served: a second mutation also completes fast.
	if err := a.SendTabCreate(0, 80, 24, "", ""); err != nil {
		t.Fatalf("A create 2: %v", err)
	}
	select {
	case <-a.TabCreated():
	case <-time.After(5 * time.Second):
		t.Fatal("A's second TabCreated never arrived — daemon path degraded with a stuck client")
	}
}
