//go:build !windows

package daemon

import (
	"bytes"
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

func writeInputFrame(t *testing.T, conn net.Conn, id uint32, payload []byte) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- protocol.WriteFrame(conn, protocol.MsgInputBytes, &protocol.InputBytes{ID: id, Bytes: payload})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write input frame: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("read loop did not consume the input frame")
	}
}

// TestReadLoopAnswersPingWhilePTYWriteBlocks reproduces a foreground
// process that has stopped reading its PTY. Input frames stay ordered,
// and the read loop must still answer a heartbeat while the first write
// is stalled.
func TestReadLoopAnswersPingWhilePTYWriteBlocks(t *testing.T) {
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
		session:    sess,
	}
	inputDone := make(chan struct{})
	go func() { c.inputLoop(); close(inputDone) }()
	writerDone := make(chan struct{})
	go func() { c.writeLoop(); close(writerDone) }()
	readDone := make(chan struct{})
	go func() { c.runReadLoop(); close(readDone) }()
	pid := tab.Term.ChildPID()
	proc := &os.Process{Pid: pid}
	t.Cleanup(func() {
		// Resume cat so it can drain any in-flight PTY write before the
		// tab is closed. This also makes failure paths release the worker.
		if pid > 0 {
			_ = proc.Signal(syscall.SIGCONT)
		}
		_ = peer.Close()
		c.shutdown()
		sess.CloseTab(tab.ID)
		for name, done := range map[string]<-chan struct{}{
			"read loop": readDone, "writer": writerDone, "input worker": inputDone,
		} {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Errorf("%s did not stop during cleanup", name)
			}
		}
	})

	if err := proc.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("stop PTY reader: %v", err)
	}
	waitForStoppedProcess(t, pid)

	first := bytes.Repeat([]byte("x"), 4<<20)
	second := []byte("ORDER_END")
	before := tab.Term.LastInput()
	writeInputFrame(t, peer, tab.ID, first)
	deadline := time.Now().Add(2 * time.Second)
	for !(tab.Term.LastInput().After(before) && c.inputBytes.Load() > 0) {
		if time.Now().After(deadline) {
			t.Fatal("large input did not remain blocked in the PTY writer")
		}
		time.Sleep(time.Millisecond)
	}
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

	if err := proc.Signal(syscall.SIGCONT); err != nil {
		t.Fatalf("resume PTY reader: %v", err)
	}
	want := append(first, second...)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := os.ReadFile(output)
		if err == nil && bytes.Equal(got, want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := os.ReadFile(output)
	t.Fatalf("PTY input did not preserve frame order: got %d bytes, want %d", len(got), len(want))
}
