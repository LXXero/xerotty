package daemon

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/protocol"
	"github.com/LXXero/xerotty/internal/terminal"
	"github.com/LXXero/xerotty/internal/testutil"
)

// TestShrinkWaitsForSettledSize guards the half-drawn tabs after a GUI
// restart: a restored window reports a provisional size, then its real
// one a few frames later. Applying a provisional shrink drops the rows
// below it, and a grow straight back leaves them blank while the app,
// back at the size it already had, has no reason to repaint. A shrink
// therefore waits resizeSettle; a size that comes back in time never
// touches the grid, and one that holds is applied.
func TestShrinkWaitsForSettledSize(t *testing.T) {
	const settle = 100 * time.Millisecond
	cfg := config.Default()
	d := New(&cfg, filepath.Join(testutil.SockDir(t), "xerottyd.sock"))
	d.SetResizeSettle(settle)
	sess := d.session("default")
	// sleep never repaints, so a lost row stays lost.
	tab, _, err := sess.NewTab(0, 80, 24, "", "", &terminal.LaunchCmd{Argv: []string{"sleep", "30"}})
	if err != nil {
		t.Fatalf("NewTab: %v", err)
	}
	defer sess.CloseTab(tab.ID)

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

	resize := func(cols, rows uint16) {
		t.Helper()
		if err := c.handleResize(&protocol.Resize{ID: tab.ID, Cols: cols, Rows: rows}); err != nil {
			t.Fatalf("handleResize: %v", err)
		}
	}
	gridIs := func(cols, rows int) bool {
		return tab.Term.Width() == cols && tab.Term.Height() == rows
	}
	wantNow := func(cols, rows int) {
		t.Helper()
		if !gridIs(cols, rows) {
			t.Fatalf("grid = %dx%d, want %dx%d", tab.Term.Width(), tab.Term.Height(), cols, rows)
		}
	}
	wantSoon := func(cols, rows int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for !gridIs(cols, rows) {
			if time.Now().After(deadline) {
				t.Fatalf("grid = %dx%d, want %dx%d", tab.Term.Width(), tab.Term.Height(), cols, rows)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	lastRow := func() string {
		g := tab.Term.SnapshotViewport()
		var sb strings.Builder
		for _, cell := range g[len(g)-1] {
			sb.WriteString(cell.Content)
		}
		return strings.TrimSpace(sb.String())
	}

	// A grow is applied at once.
	resize(120, 40)
	wantNow(120, 40)
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(tab.Term.Emu, "\x1b[%d;1HROW%02d", i, i)
	}

	// A provisional shrink and straight back: the grid never moves and
	// the bottom row survives.
	resize(120, 20)
	wantNow(120, 40)
	resize(120, 40)
	time.Sleep(3 * settle)
	wantNow(120, 40)
	if got := lastRow(); got != "ROW40" {
		t.Fatalf("bottom row = %q after a provisional shrink, want ROW40", got)
	}

	// A shrink that holds is applied after the wait.
	resize(100, 30)
	wantNow(120, 40)
	wantSoon(100, 30)

	// Each further shrink restarts the wait; the last size wins.
	resize(90, 28)
	time.Sleep(settle / 2)
	resize(80, 24)
	time.Sleep(settle / 2)
	wantNow(100, 30)
	wantSoon(80, 24)

	// A grow cancels a pending shrink and applies at once.
	resize(70, 20)
	resize(130, 45)
	wantNow(130, 45)
	time.Sleep(3 * settle)
	wantNow(130, 45)
}
