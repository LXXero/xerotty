package daemon_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
)

// TestDaemonChildExitFiresFrame is the regression test for the bug
// where running `exit` in a shell left the client hanging forever:
// daemon's waitChild detected the reap but never shipped
// MsgChildExit, so `xerotty connect` (and the GUI in daemon mode)
// had no signal that anything happened.
//
// Setup: spawn daemon, attach, send `exit\r` to the shell, then
// wait for a ChildExit frame. Must arrive within a couple seconds.
func TestDaemonChildExitFiresFrame(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)

	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	c, err := clientproto.Dial(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if _, err := c.Hello("exit-test"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	go c.Run()
	if err := c.Attach("", true); err != nil {
		t.Fatalf("attach: %v", err)
	}
	attached := <-c.Attached()
	tabID := attached.Tabs[0].ID

	// Drain the initial CellFull so the channel doesn't backpressure
	// the daemon's send loop. Discard everything we don't care about.
	go func() {
		for {
			select {
			case <-c.CellFull():
			case <-c.CellDiff():
			case <-c.Cursor():
			case <-c.Closed():
				return
			}
		}
	}()

	// Tell the shell to exit. Most shells take a beat to flush
	// before reaping; the 5s timeout covers that comfortably.
	if err := c.SendInput(tabID, []byte("exit\r")); err != nil {
		t.Fatalf("send exit: %v", err)
	}

	select {
	case ce := <-c.ChildExit():
		if ce.ID != tabID {
			t.Errorf("ChildExit for tab %d, want %d", ce.ID, tabID)
		}
		// Exit code can be 0 or non-zero depending on the shell —
		// just confirm we got the frame.
	case <-time.After(5 * time.Second):
		t.Fatal("no MsgChildExit within 5s after shell exit — this is the regression")
	}
}
