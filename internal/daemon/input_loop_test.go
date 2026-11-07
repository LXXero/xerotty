//go:build !windows

package daemon

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/protocol"
	"github.com/LXXero/xerotty/internal/terminal"
)

func newInputTestTab(t *testing.T, launch *terminal.LaunchCmd) (*Daemon, *Session, *Tab) {
	t.Helper()
	cfg := config.Default()
	d := New(&cfg, "")
	sess := d.session("default")
	tab, _, err := sess.NewTab(0, 80, 24, "", "", launch)
	if err != nil {
		t.Skipf("PTY unavailable: %v", err)
	}
	t.Cleanup(func() { sess.CloseTab(tab.ID) })
	return d, sess, tab
}

func waitForForeground(t *testing.T, tab *Tab, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := tab.Term.ForegroundProcessName(); got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("foreground process = %q, want %q", tab.Term.ForegroundProcessName(), want)
}

func waitForStoppedProcess(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		state, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if err == nil && strings.HasPrefix(strings.TrimSpace(string(state)), "T") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d did not stop", pid)
}

func writeTestFrame(t *testing.T, conn net.Conn, typ protocol.MsgType, msg protocol.Msg) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- protocol.WriteFrame(conn, typ, msg) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write %v frame: %v", typ, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("read loop did not consume the %v frame", typ)
	}
}

func writeInputFrame(t *testing.T, conn net.Conn, id uint32, payload []byte) {
	t.Helper()
	writeTestFrame(t, conn, protocol.MsgInputBytes, &protocol.InputBytes{ID: id, Bytes: payload})
}

// blockedInputConn is a clientConn wired to a PTY tab whose reader
// (cat) is stopped, with a large first frame stuck in the input worker.
type blockedInputConn struct {
	sess      *Session
	tab       *Tab
	c         *clientConn
	peer      net.Conn
	proc      *os.Process
	output    string
	readDone  chan struct{}
	inputDone chan struct{}
}

func startBlockedInputConn(t *testing.T, first []byte) *blockedInputConn {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	output := filepath.Join(t.TempDir(), "input")
	script := `stty -echo -icanon; : > "$1"; exec cat > "$2"`
	d, sess, tab := newInputTestTab(t, &terminal.LaunchCmd{Argv: []string{
		"sh", "-c", script, "sh", ready, output,
	}})
	waitForForeground(t, tab, "cat")
	if _, err := os.Stat(ready); err != nil {
		t.Fatalf("PTY child did not finish setup: %v", err)
	}

	server, peer := net.Pipe()
	c := &clientConn{
		daemon:     d,
		conn:       server,
		reader:     protocol.NewFrameReader(server),
		outCh:      make(chan outFrame, outQueueCap),
		coalesceCh: make(chan struct{}, 1),
		outDone:    make(chan struct{}),
		dw:         &deadlineWriter{conn: server, window: writeProgressWindow},
		inputCh:    make(chan inputFrame, inputQueueCap),
		inputStop:  make(chan struct{}),
		drainAfter: inputDrainWindow,
		session:    sess,
	}
	b := &blockedInputConn{
		sess: sess, tab: tab, c: c, peer: peer, output: output,
		readDone: make(chan struct{}), inputDone: make(chan struct{}),
	}
	go func() { c.inputLoop(); close(b.inputDone) }()
	writerDone := make(chan struct{})
	go func() { c.writeLoop(); close(writerDone) }()
	go func() { c.runReadLoop(); close(b.readDone) }()
	pid := tab.Term.ChildPID()
	b.proc = &os.Process{Pid: pid}
	t.Cleanup(func() {
		// Resume cat so it can drain any in-flight PTY write before the
		// tab is closed. This also makes failure paths release the worker.
		if pid > 0 {
			_ = b.proc.Signal(syscall.SIGCONT)
		}
		_ = peer.Close()
		c.shutdown()
		for name, done := range map[string]<-chan struct{}{
			"read loop": b.readDone, "writer": writerDone, "input worker": b.inputDone,
		} {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Errorf("%s did not stop during cleanup", name)
			}
		}
		sess.CloseTab(tab.ID)
	})

	if err := b.proc.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("stop PTY reader: %v", err)
	}
	waitForStoppedProcess(t, pid)

	before := tab.Term.LastInput()
	writeInputFrame(t, peer, tab.ID, first)
	deadline := time.Now().Add(2 * time.Second)
	for !(tab.Term.LastInput().After(before) && c.inputBytes.Load() > 0) {
		if time.Now().After(deadline) {
			t.Fatal("large input did not remain blocked in the PTY writer")
		}
		time.Sleep(time.Millisecond)
	}
	return b
}

func (b *blockedInputConn) resume(t *testing.T) {
	t.Helper()
	if err := b.proc.Signal(syscall.SIGCONT); err != nil {
		t.Fatalf("resume PTY reader: %v", err)
	}
}

func waitForPTYOutput(t *testing.T, path string, want []byte) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := os.ReadFile(path)
		if err == nil && bytes.Equal(got, want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := os.ReadFile(path)
	t.Fatalf("PTY input mismatch: got %d bytes, want %d", len(got), len(want))
}

// TestReadLoopAnswersPingWhilePTYWriteBlocks reproduces a foreground
// process that has stopped reading its PTY. Input frames stay ordered,
// and the read loop must still answer a heartbeat while the first write
// is stalled.
func TestReadLoopAnswersPingWhilePTYWriteBlocks(t *testing.T) {
	first := bytes.Repeat([]byte("x"), 4<<20)
	b := startBlockedInputConn(t, first)
	peer, tab := b.peer, b.tab
	second := []byte("ORDER_END")
	writeInputFrame(t, peer, tab.ID, second)

	pongRead := make(chan struct {
		typ  protocol.MsgType
		pong protocol.Pong
		err  error
	}, 1)
	go func() {
		typ, body, err := protocol.NewFrameReader(peer).ReadFrame()
		result := struct {
			typ  protocol.MsgType
			pong protocol.Pong
			err  error
		}{typ: typ, err: err}
		if err == nil {
			_, result.err = result.pong.UnmarshalMsg(body)
		}
		pongRead <- result
	}()
	if err := protocol.WriteFrame(peer, protocol.MsgPing, &protocol.Ping{Nonce: 42}); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	select {
	case got := <-pongRead:
		if got.err != nil {
			t.Fatalf("read pong: %v", got.err)
		}
		if got.typ != protocol.MsgPong || got.pong.Nonce != 42 {
			t.Fatalf("pong = (%v, %+v), want nonce 42", got.typ, got.pong)
		}
	case <-time.After(time.Second):
		t.Fatal("no pong while the PTY input write was blocked")
	}

	b.resume(t)
	waitForPTYOutput(t, b.output, append(first, second...))
}

// TestInputQueueOverflowDrainsAcceptedFrames fills the input queue
// behind a blocked PTY write. The overflowing frame closes the
// connection, but every frame accepted before it still reaches the PTY
// in order once the reader resumes, and the worker then exits.
func TestInputQueueOverflowDrainsAcceptedFrames(t *testing.T) {
	first := bytes.Repeat([]byte("x"), 4<<20)
	b := startBlockedInputConn(t, first)
	want := append([]byte(nil), first...)
	for i := 0; i < inputQueueCap; i++ {
		f := []byte(fmt.Sprintf("<%02d>", i))
		writeInputFrame(t, b.peer, b.tab.ID, f)
		want = append(want, f...)
	}
	writeInputFrame(t, b.peer, b.tab.ID, []byte("OVERFLOW"))
	select {
	case <-b.readDone:
	case <-time.After(3 * time.Second):
		t.Fatal("read loop did not stop on a full input queue")
	}
	b.c.shutdown() // serveConn tears the connection down after the read loop
	b.resume(t)
	waitForPTYOutput(t, b.output, want)
	select {
	case <-b.inputDone:
	case <-time.After(3 * time.Second):
		t.Fatal("input worker did not exit after draining")
	}
}

// TestProposalApprovalKeptWhenInputQueueFull approves a proposal while
// the input queue is full. The approval cannot be queued, so the action
// must stay pending instead of being lost.
func TestProposalApprovalKeptWhenInputQueueFull(t *testing.T) {
	b := startBlockedInputConn(t, bytes.Repeat([]byte("x"), 4<<20))
	for i := 0; i < inputQueueCap; i++ {
		writeInputFrame(t, b.peer, b.tab.ID, []byte("q"))
	}
	b.sess.QueueProposedInput(b.tab.ID, []byte("APPROVED"))
	writeTestFrame(t, b.peer, protocol.MsgProposalResolve, &protocol.ProposalResolve{Index: 0, Approve: true})
	select {
	case <-b.readDone:
	case <-time.After(3 * time.Second):
		t.Fatal("read loop did not stop on a full input queue")
	}
	pending := b.sess.PendingProposals()
	if len(pending) != 1 || string(pending[0].Bytes) != "APPROVED" || pending[0].TabID != b.tab.ID {
		t.Fatalf("pending proposals = %+v, want the approved action kept", pending)
	}
}

func TestRestoreProposalKeepsQueueBoundedAndPreservesApproval(t *testing.T) {
	s := &Session{}
	for i := 0; i < proposedCap; i++ {
		s.QueueProposedInput(1, []byte(fmt.Sprintf("proposal-%03d", i)))
	}

	found, approved, _ := s.takeProposal(0)
	if !found || string(approved.Bytes) != "proposal-000" {
		t.Fatalf("taken proposal = %q, found %v; want proposal-000", approved.Bytes, found)
	}
	// Another proposal takes the freed slot before the failed approval is
	// restored, matching the race between proposal production and enqueue.
	s.QueueProposedInput(1, []byte("new-proposal"))
	s.restoreProposal(0, approved)

	pending := s.PendingProposals()
	if len(pending) != proposedCap {
		t.Fatalf("pending proposals = %d, want bounded at %d", len(pending), proposedCap)
	}
	if string(pending[0].Bytes) != "proposal-000" {
		t.Fatalf("first pending proposal = %q, want restored approval", pending[0].Bytes)
	}
}

// closeBlockedConn closes the client side the way a disconnect does:
// the read loop exits (closing inputCh) and serveConn shuts the conn
// down, then waits for the input worker with the given drain window.
// It returns a channel closed when awaitInputWorker returns.
func closeBlockedConn(t *testing.T, b *blockedInputConn, window time.Duration) <-chan struct{} {
	t.Helper()
	_ = b.peer.Close()
	select {
	case <-b.readDone:
	case <-time.After(3 * time.Second):
		t.Fatal("read loop did not stop when the client closed")
	}
	b.c.shutdown()
	b.c.drainAfter = window
	awaited := make(chan struct{})
	go func() { b.c.awaitInputWorker(b.inputDone); close(awaited) }()
	return awaited
}

// TestDisconnectCancelsStalledInput closes a client whose PTY write is
// blocked on a stopped foreground process. Once the write makes no
// progress for the drain window, the worker must exit and release every
// queued byte while the process is STILL stopped, and the input queued
// behind the blocked write must never reach the PTY.
func TestDisconnectCancelsStalledInput(t *testing.T) {
	first := bytes.Repeat([]byte("x"), 4<<20)
	b := startBlockedInputConn(t, first)
	for i := 0; i < 8; i++ {
		writeInputFrame(t, b.peer, b.tab.ID, []byte(fmt.Sprintf("<q%d>", i)))
	}
	awaited := closeBlockedConn(t, b, 200*time.Millisecond)
	select {
	case <-awaited:
	case <-time.After(3 * time.Second):
		t.Fatal("serveConn's wait for the input worker did not end while the PTY write was blocked")
	}
	select {
	case <-b.inputDone:
	default:
		t.Fatal("awaitInputWorker returned before the input worker exited")
	}
	if n := b.c.inputBytes.Load(); n != 0 {
		t.Fatalf("queued input bytes after cancel = %d, want 0", n)
	}

	// Let the abandoned in-kernel write finish, then check that nothing
	// queued behind it was written.
	b.resume(t)
	var got []byte
	deadline := time.Now().Add(3 * time.Second)
	for prev := -1; time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		got, _ = os.ReadFile(b.output)
		if len(got) == prev {
			break
		}
		prev = len(got)
	}
	if bytes.Contains(got, []byte("<q")) {
		t.Fatal("input queued behind the cancelled write reached the PTY")
	}
	if len(got) >= len(first) {
		t.Fatalf("PTY got %d bytes, want the cancelled write cut short of %d", len(got), len(first))
	}
}

// TestDisconnectDrainsInputWhilePTYKeepsUp closes a client while input
// is still queued, and the PTY reader resumes inside the drain window.
// Every accepted frame must reach the PTY in order before the worker
// exits.
func TestDisconnectDrainsInputWhilePTYKeepsUp(t *testing.T) {
	first := bytes.Repeat([]byte("x"), 4<<20)
	b := startBlockedInputConn(t, first)
	want := append([]byte(nil), first...)
	for i := 0; i < 8; i++ {
		f := []byte(fmt.Sprintf("<q%d>", i))
		writeInputFrame(t, b.peer, b.tab.ID, f)
		want = append(want, f...)
	}
	awaited := closeBlockedConn(t, b, 2*time.Second)
	b.resume(t)
	select {
	case <-awaited:
	case <-time.After(5 * time.Second):
		t.Fatal("input worker did not exit after draining")
	}
	waitForPTYOutput(t, b.output, want)
	if n := b.c.inputBytes.Load(); n != 0 {
		t.Fatalf("queued input bytes after drain = %d, want 0", n)
	}
}
