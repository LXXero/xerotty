package daemonsource

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/protocol"
)

func (f *fakeDaemon) sendError(reqID uint64, msg string) {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	_ = protocol.WriteFrame(f.conn, protocol.MsgError, &protocol.Error{
		Code: 3, Message: msg, ReqID: reqID,
	})
}

// TestNewTabInFailsFastOnDaemonError verifies a daemon MsgError that
// echoes the create's ReqID fails NewTabIn immediately. NewTabIn runs
// on the GUI's UI thread, so waiting out createTimeout for a create the
// daemon already refused froze every window for ~10s.
func TestNewTabInFailsFastOnDaemonError(t *testing.T) {
	cConn, sConn := net.Pipe()
	defer cConn.Close()
	defer sConn.Close()
	fake := newFakeDaemon(sConn)

	cli := clientproto.Wrap(cConn)
	go cli.Run()
	h := NewHub(cli)
	defer h.Stop()
	h.createTimeout = 30 * time.Second // a timeout-path pass would be obvious

	go func() {
		c := <-fake.creates
		fake.sendError(c.ReqID, "chdir /home/nobody: no such file or directory")
	}()

	start := time.Now()
	_, err := h.NewTabIn(0, 80, 24, "/home/nobody", nil)
	if err == nil {
		t.Fatal("expected an error when the daemon refuses the create")
	}
	if !strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("error should carry the daemon's message, got: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("NewTabIn took %v — waited for the timeout instead of failing fast", d)
	}
}

// TestUncorrelatedErrorDoesNotFailCreate verifies an error WITHOUT a
// matching ReqID (an older daemon, or an unrelated protocol error)
// leaves a pending create alone — it still completes on its real ack.
func TestUncorrelatedErrorDoesNotFailCreate(t *testing.T) {
	cConn, sConn := net.Pipe()
	defer cConn.Close()
	defer sConn.Close()
	fake := newFakeDaemon(sConn)

	cli := clientproto.Wrap(cConn)
	go cli.Run()
	h := NewHub(cli)
	defer h.Stop()

	go func() {
		c := <-fake.creates
		fake.sendError(0, "something unrelated")
		fake.sendError(c.ReqID+1000, "someone else's request")
		fake.sendTabCreated(c.ReqID, 7)
	}()

	src, err := h.NewTabIn(0, 80, 24, "", nil)
	if err != nil {
		t.Fatalf("create failed on an uncorrelated error: %v", err)
	}
	if src.TabID() != 7 {
		t.Fatalf("adopted tab %d, want 7", src.TabID())
	}
}
