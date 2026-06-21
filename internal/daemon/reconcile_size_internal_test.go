package daemon

import (
	"net"
	"path/filepath"
	"testing"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/protocol"
)

// TestReconcileTabSizeSmallestWins guards the multi-client resize war:
// the shared grid follows whichever attached client resized MOST
// RECENTLY (a genuine resize, not the 0.5s same-size re-request that
// would otherwise ping-pong two clients). On detach the grid hands
// off to the next-most-recently-resized client.
//
// White-box so it can drive handleResize / unsubscribe directly,
// avoiding the timing-dependent networking a real two-client stress
// test would need.
func TestReconcileTabSizeMostRecentWins(t *testing.T) {
	cfg := config.Default()
	d := New(&cfg, filepath.Join(t.TempDir(), "xerottyd.sock"))
	sess := d.session("default")
	tab, _, err := sess.NewTab(0, 80, 24, "", "", nil)
	if err != nil {
		t.Fatalf("NewTab: %v", err)
	}
	defer sess.CloseTab(tab.ID)

	mkClient := func() *clientConn {
		cConn, sConn := net.Pipe()
		t.Cleanup(func() { cConn.Close(); sConn.Close() })
		go func() {
			buf := make([]byte, 4096)
			for {
				if _, err := sConn.Read(buf); err != nil {
					return
				}
			}
		}()
		c := &clientConn{daemon: d, conn: cConn, session: sess}
		d.registerClient(c)
		c.subsMu.Lock()
		c.subSession = sess
		c.subsMu.Unlock()
		c.subscribe(sess, tab)
		return c
	}

	cA := mkClient()
	cB := mkClient()

	resize := func(c *clientConn, cols, rows uint16) {
		if err := c.handleResize(&protocol.Resize{ID: tab.ID, Cols: cols, Rows: rows}); err != nil {
			t.Fatalf("handleResize: %v", err)
		}
	}
	wantSize := func(cols, rows int) {
		t.Helper()
		if w, h := tab.Term.Width(), tab.Term.Height(); w != cols || h != rows {
			t.Fatalf("grid = %dx%d, want %dx%d", w, h, cols, rows)
		}
	}

	// A resizes to 120x40 — only sizer, it wins.
	resize(cA, 120, 40)
	wantSize(120, 40)

	// B resizes to 80x24 — more RECENT resize wins, grid follows B
	// (NOT min-wins: B is smaller here, but the point is recency).
	resize(cB, 80, 24)
	wantSize(80, 24)

	// A re-demands its SAME 120x40 every frame (app.resizeReq while
	// mismatched). That's a level, not an edge — it must be a no-op so
	// the two don't ping-pong (the resize-war flashing). Grid stays B's.
	resize(cA, 120, 40)
	wantSize(80, 24)
	resize(cA, 120, 40)
	wantSize(80, 24)

	// A GENUINELY resizes — to something BIGGER than B. Most-recent
	// wins, so the grid follows A even though it's larger (this is the
	// case that distinguishes recency from smallest-wins). B's window
	// just letterboxes; the user is driving A right now.
	resize(cA, 150, 50)
	wantSize(150, 50)

	// B's periodic re-request of its old 80x24 is a level → no-op.
	resize(cB, 80, 24)
	wantSize(150, 50)

	// A detaches: the running winner drops out, so the grid hands off
	// to the next-most-recently-resized attached client (B).
	cA.unsubscribe(tab.ID)
	wantSize(80, 24)
}
