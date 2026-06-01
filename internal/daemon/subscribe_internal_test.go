package daemon

import (
	"net"
	"path/filepath"
	"testing"

	"github.com/LXXero/xerotty/internal/config"
)

// TestSubscribeRespectsAttachState is the deterministic guard for the
// create-during-detach race (MED 1): subscribe must add a publish loop
// ONLY while the connection is attached to the tab's session
// (subSession set under subsMu). A subscribe arriving after teardown —
// the window between another connection's stopAllSubs and its
// unregister/sessionName clear — must be a no-op, or it leaks a publish
// loop whose cancel was already closed.
//
// White-box (package daemon) so it can drive clientConn.subscribe /
// stopAllSubs directly, avoiding the timing-dependent multi-client
// networking a stress test would need.
func TestSubscribeRespectsAttachState(t *testing.T) {
	cfg := config.Default()
	d := New(&cfg, filepath.Join(t.TempDir(), "xerottyd.sock")) // not Run; we drive subscribe directly
	sess := d.session("default")
	tab, _, err := sess.NewTab(0, 80, 24, "")
	if err != nil {
		t.Fatalf("NewTab: %v", err)
	}
	defer sess.CloseTab(tab.ID)

	// The publish loop writes frames to the conn; give it a pipe whose
	// far end is drained so it never blocks.
	cConn, sConn := net.Pipe()
	defer cConn.Close()
	defer sConn.Close()
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := sConn.Read(buf); err != nil {
				return
			}
		}
	}()
	c := &clientConn{daemon: d, conn: cConn}

	subCount := func() int {
		c.subsMu.Lock()
		defer c.subsMu.Unlock()
		return len(c.subs)
	}

	// Not attached (subSession nil) → subscribe is a no-op.
	c.subscribe(sess, tab)
	if n := subCount(); n != 0 {
		t.Fatalf("subscribed while not attached: got %d subs, want 0", n)
	}

	// Attached → subscribe adds the sub + starts the publish loop.
	c.subsMu.Lock()
	c.subSession = sess
	c.subsMu.Unlock()
	c.subscribe(sess, tab)
	if n := subCount(); n != 1 {
		t.Fatalf("did not subscribe while attached: got %d subs, want 1", n)
	}

	// Teardown clears subSession (under subsMu) and cancels subs. A
	// subscribe racing in AFTER teardown — exactly the detach/disconnect
	// gap — must be rejected, not leak a publish loop.
	c.stopAllSubs()
	if n := subCount(); n != 0 {
		t.Fatalf("stopAllSubs left %d subs, want 0", n)
	}
	c.subscribe(sess, tab)
	if n := subCount(); n != 0 {
		t.Fatalf("subscribe added a sub after teardown: got %d, want 0 (leak)", n)
	}
}
