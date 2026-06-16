package daemon

import (
	"net"
	"path/filepath"
	"testing"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/protocol"
)

// TestReconcileTabSizeSmallestWins guards the multi-client resize war:
// two differently-sized GUIs attached to one daemon tab must settle on
// the SMALLEST grid (so both can display it), and the larger client's
// repeated re-requests must NOT thrash the shared PTY. When the small
// client detaches, the size grows back to the remaining client.
//
// White-box so it can drive handleResize / unsubscribe directly,
// avoiding the timing-dependent networking a real two-client stress
// test would need.
func TestReconcileTabSizeSmallestWins(t *testing.T) {
	cfg := config.Default()
	d := New(&cfg, filepath.Join(t.TempDir(), "xerottyd.sock"))
	sess := d.session("default")
	tab, _, err := sess.NewTab(0, 80, 24, "", "")
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

	cBig := mkClient()
	cSmall := mkClient()

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

	// Big client asks for 120x40 — it's the only sizer, so it wins.
	resize(cBig, 120, 40)
	wantSize(120, 40)

	// Small client asks for 80x24 — smallest now wins so both fit.
	resize(cSmall, 80, 24)
	wantSize(80, 24)

	// Big client re-demands 120x40 every frame (app.resizeReq). The min
	// is still 80x24, so this must be a no-op — no thrash back to 120.
	resize(cBig, 120, 40)
	wantSize(80, 24)

	// Small client detaches: its constraint lifts, grid grows back to
	// the big client's request.
	cSmall.unsubscribe(tab.ID)
	wantSize(120, 40)
}
