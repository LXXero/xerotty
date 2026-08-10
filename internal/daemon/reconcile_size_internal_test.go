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

	// A's first size report — no owner yet, so it wins.
	resize(cA, 120, 40)
	wantSize(120, 40)

	// B's FIRST report (0×0 → 80x24) is attach bookkeeping, not a user
	// resize — it must NOT steal the grid from A (the sleeping-laptop
	// reattach bug). B's size is recorded for fallback only.
	resize(cB, 80, 24)
	wantSize(120, 40)

	// B GENUINELY resizes (an edge from its recorded size) — a real
	// user action, most-recent wins, grid follows B.
	resize(cB, 90, 28)
	wantSize(90, 28)

	// A re-demands its SAME 120x40 every frame (app.resizeReq while
	// mismatched). That's a level, not an edge — it must be a no-op so
	// the two don't ping-pong (the resize-war flashing). Grid stays B's.
	resize(cA, 120, 40)
	wantSize(90, 28)
	resize(cA, 120, 40)
	wantSize(90, 28)

	// A GENUINELY resizes — to something BIGGER than B. Most-recent
	// wins, so the grid follows A even though it's larger (this is the
	// case that distinguishes recency from smallest-wins). B's window
	// just letterboxes; the user is driving A right now.
	resize(cA, 150, 50)
	wantSize(150, 50)

	// B's periodic re-request of its old 90x28 is a level → no-op.
	resize(cB, 90, 28)
	wantSize(150, 50)

	// A detaches: the running winner drops out, so the grid hands off
	// to the next-most-recently-resized attached client (B).
	cA.unsubscribe(tab.ID)
	wantSize(90, 28)
}

// TestSizeOwnershipFollowsInput: typing on a machine claims the shared
// grid the same way resizing does — the "switch boxes and just work"
// path. A mouse report must NOT claim (keystroke + resize, not click),
// and the owner typing must be a no-op (no thrash).
func TestSizeOwnershipFollowsInput(t *testing.T) {
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
	wantSize := func(cols, rows int) {
		t.Helper()
		if w, h := tab.Term.Width(), tab.Term.Height(); w != cols || h != rows {
			t.Fatalf("grid = %dx%d, want %dx%d", w, h, cols, rows)
		}
	}

	cA := mkClient()
	cB := mkClient()

	// Both report their window sizes. A seeds first (no owner → owns);
	// B's first report is attach bookkeeping and must not steal.
	if err := cA.handleResize(&protocol.Resize{ID: tab.ID, Cols: 200, Rows: 50}); err != nil {
		t.Fatal(err)
	}
	if err := cB.handleResize(&protocol.Resize{ID: tab.ID, Cols: 100, Rows: 30}); err != nil {
		t.Fatal(err)
	}
	wantSize(200, 50)

	// User switches to machine B and TYPES — B claims, grid reflows to
	// B's window even with no resize.
	if err := cB.handleInput(tab.ID, []byte("ls\r")); err != nil {
		t.Fatal(err)
	}
	wantSize(100, 30)

	// User switches back to A and types — A claims it back.
	if err := cA.handleInput(tab.ID, []byte("echo hi\r")); err != nil {
		t.Fatal(err)
	}
	wantSize(200, 50)

	// A keeps typing — already the owner, must be a no-op (no thrash).
	if err := cA.handleInput(tab.ID, []byte("pwd\r")); err != nil {
		t.Fatal(err)
	}
	wantSize(200, 50)

	// A MOUSE report from B must NOT claim (not a keystroke) — grid
	// stays A's. SGR mouse: ESC [ < 0 ; 5 ; 5 M.
	if err := cB.handleInput(tab.ID, []byte("\x1b[<0;5;5M")); err != nil {
		t.Fatal(err)
	}
	wantSize(200, 50)

	// A real keystroke on B claims it back.
	if err := cB.handleInput(tab.ID, []byte("q")); err != nil {
		t.Fatal(err)
	}
	wantSize(100, 30)
}

// TestReattachDoesNotStealSize is the sleeping-laptop regression: a
// client whose connection cycles (SSH dropping and re-attaching while
// the machine dozes) re-seeds its size on every round trip. Each seed
// must record the size WITHOUT claiming ownership — the human typing
// on the other machine keeps the grid, no matter how many times the
// zombie reconnects.
func TestReattachDoesNotStealSize(t *testing.T) {
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
	wantSize := func(cols, rows int) {
		t.Helper()
		if w, h := tab.Term.Width(), tab.Term.Height(); w != cols || h != rows {
			t.Fatalf("grid = %dx%d, want %dx%d", w, h, cols, rows)
		}
	}

	// The local GUI attaches, seeds, and owns the grid.
	local := mkClient()
	if err := local.handleResize(&protocol.Resize{ID: tab.ID, Cols: 190, Rows: 45}); err != nil {
		t.Fatal(err)
	}
	wantSize(190, 45)

	// The laptop cycles: attach, seed, drop — several times. The grid
	// must never budge.
	for i := 0; i < 3; i++ {
		laptop := mkClient()
		if err := laptop.handleResize(&protocol.Resize{ID: tab.ID, Cols: 100, Rows: 30}); err != nil {
			t.Fatal(err)
		}
		wantSize(190, 45)
		laptop.unsubscribe(tab.ID)
		d.unregisterClient(laptop)
		wantSize(190, 45)
	}

	// A human actually typing on the laptop claims normally.
	laptop := mkClient()
	if err := laptop.handleResize(&protocol.Resize{ID: tab.ID, Cols: 100, Rows: 30}); err != nil {
		t.Fatal(err)
	}
	wantSize(190, 45)
	if err := laptop.handleInput(tab.ID, []byte("ls\r")); err != nil {
		t.Fatal(err)
	}
	wantSize(100, 30)
}
