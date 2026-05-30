package daemon_test

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
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

// attachedBy reports whether a client with the given ClientID is
// currently registered on the daemon.
func attachedBy(d *daemon.Daemon, id string) bool {
	for _, ac := range d.AttachedClients() {
		if ac.ClientID == id {
			return true
		}
	}
	return false
}

// TestFlowingReaderNotReaped verifies finding #1: a client that keeps
// READING the daemon's output (so writes keep making progress) is NOT
// reaped even though it never pongs. Write progress overrides pong
// staleness — a slow-but-flowing reader draining a backlog, whose pong
// is merely late, must not be killed.
func TestFlowingReaderNotReaped(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)
	d.SetHeartbeat(25*time.Millisecond, 120*time.Millisecond)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	fr := protocol.NewFrameReader(conn)
	if err := protocol.WriteFrame(conn, protocol.MsgHello, &protocol.Hello{
		Version: protocol.ProtocolVersion, ClientID: "flowing",
	}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	readFrameExpect(t, fr, protocol.MsgHelloAck)
	if err := protocol.WriteFrame(conn, protocol.MsgAttach, &protocol.Attach{NewIfMissing: true}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	// Read+discard EVERYTHING forever (incl. the heartbeat pings) but
	// never pong. Writes keep flowing, so we must stay alive.
	go func() {
		for {
			if _, _, err := fr.ReadFrame(); err != nil {
				return
			}
		}
	}()

	// Well past several windows, still attached.
	time.Sleep(600 * time.Millisecond)
	if !attachedBy(d, "flowing") {
		t.Fatal("a flowing (still-reading) client was wrongly reaped because it didn't pong — finding 1 regression")
	}
}

// TestStoppedReaderReaped verifies finding #2: a client that STOPS
// reading (so the daemon's writer wedges) and stops ponging IS reaped —
// even while it keeps SENDING frames. Generic inbound no longer counts
// as liveness; only a real pong (or write progress) does. Served over
// an unbuffered net.Pipe so the writer wedges immediately, exercising
// the heartbeat rather than the layer-3 deadline.
func TestStoppedReaderReaped(t *testing.T) {
	cfg := config.Default()
	d := daemon.New(&cfg, "")
	d.SetHeartbeat(25*time.Millisecond, 120*time.Millisecond)

	cConn, sConn := net.Pipe()
	go d.ServeConn(sConn)
	defer cConn.Close()
	fr := protocol.NewFrameReader(cConn)
	if err := protocol.WriteFrame(cConn, protocol.MsgHello, &protocol.Hello{
		Version: protocol.ProtocolVersion, ClientID: "stopped",
	}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	readFrameExpect(t, fr, protocol.MsgHelloAck)
	if err := protocol.WriteFrame(cConn, protocol.MsgAttach, &protocol.Attach{NewIfMissing: true}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	// From here we never READ again — the daemon's writer wedges on its
	// next frame. But we keep SENDING (focus pings) to prove that
	// inbound traffic does NOT keep us alive.
	stopSend := make(chan struct{})
	defer close(stopSend)
	go func() {
		for {
			select {
			case <-stopSend:
				return
			case <-time.After(25 * time.Millisecond):
				if err := protocol.WriteFrame(cConn, protocol.MsgTabFocus, &protocol.TabFocus{ID: 1}); err != nil {
					return
				}
			}
		}
	}()

	deadline := time.After(3 * time.Second)
	for attachedBy(d, "stopped") {
		select {
		case <-deadline:
			t.Fatal("a stopped-reading client (still sending, never ponging) was not reaped — finding 2")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestHandshakeHangNoZombie verifies finding #3: a client that sends
// Hello then never reads (so the synchronous HelloAck write would hang
// before the writer/heartbeat goroutines exist) does NOT leave a
// registered-but-stuck conn — the handshake watchdog closes it.
func TestHandshakeHangNoZombie(t *testing.T) {
	cfg := config.Default()
	d := daemon.New(&cfg, "")
	d.SetHeartbeat(25*time.Millisecond, 120*time.Millisecond)

	cConn, sConn := net.Pipe()
	go d.ServeConn(sConn)
	defer cConn.Close()
	// Send Hello, then NEVER read — the daemon's HelloAck write blocks
	// on the unbuffered pipe. The watchdog must close the conn and the
	// deferred unregister must clean up.
	if err := protocol.WriteFrame(cConn, protocol.MsgHello, &protocol.Hello{
		Version: protocol.ProtocolVersion, ClientID: "hang",
	}); err != nil {
		t.Fatalf("hello: %v", err)
	}

	// No zombie may persist past the watchdog window. (Without it, the
	// HelloAck write blocks forever and "hang" stays registered.)
	deadline := time.After(2 * time.Second)
	for {
		if !attachedBy(d, "hang") && len(d.AttachedClients()) == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("a hung handshake left a registered zombie conn — finding 3")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestResponsiveClientNotReaped is the converse of the reap test: a
// client that keeps ponging (clientproto auto-replies to pings) stays
// attached well past the dead window, even with no user traffic.
func TestResponsiveClientNotReaped(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)
	d.SetHeartbeat(30*time.Millisecond, 150*time.Millisecond)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	// mustDial returns a clientproto client whose Run loop auto-pongs.
	c := mustDial(t, sockPath, "alive")
	defer c.Close()
	<-c.Attached()
	go drainClient(c, false, false)

	// Idle (no input/resize) for several dead-windows. Pongs alone must
	// keep it alive.
	time.Sleep(700 * time.Millisecond)

	found := false
	for _, ac := range d.AttachedClients() {
		if ac.ClientID == "alive" {
			found = true
		}
	}
	if !found {
		t.Fatal("a ponging client was wrongly reaped (heartbeat false positive)")
	}
}
