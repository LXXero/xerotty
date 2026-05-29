package daemon_test

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/protocol"
)

// startDaemon spins up a daemon on a temp socket and returns it plus a
// teardown. Shared by the topology tests.
func startDaemon(t *testing.T) (*daemon.Daemon, string) {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	t.Cleanup(func() { _ = d.Stop(); <-doneRun })
	time.Sleep(50 * time.Millisecond) // listener bind
	return d, sockPath
}

// drainClient consumes a client's render/control frames in the
// background so Client.Run never back-pressures. The named channels
// are LEFT for the test to read (passing nil to the select makes that
// case never fire). Stops when the client closes.
func drainClient(c *clientproto.Client, keepTopology, keepSbCleared bool) {
	var topoCh <-chan *protocol.TopologyChanged
	if !keepTopology {
		topoCh = c.Topology()
	}
	var sbCh <-chan *protocol.ScrollbackCleared
	if !keepSbCleared {
		sbCh = c.ScrollbackCleared()
	}
	for {
		select {
		case <-c.Closed():
			return
		case <-c.CellFull():
		case <-c.CellDiff():
		case <-c.Cursor():
		case <-c.Title():
		case <-c.Bell():
		case <-c.ChildExit():
		case <-c.TabState():
		case <-c.ScrollbackAppend():
		case <-c.TabCreated():
		case <-c.WindowCreated():
		case <-c.ClipboardSet():
		case <-c.ProposalsChanged():
		case <-c.Errors():
		case <-topoCh:
		case <-sbCh:
		}
	}
}

// waitTopology reads the next MsgTopologyChanged whose Revision is
// greater than afterRev (skipping stale/duplicate ones), failing the
// test on timeout.
func waitTopology(t *testing.T, c *clientproto.Client, afterRev uint64, dur time.Duration) *protocol.TopologyChanged {
	t.Helper()
	deadline := time.After(dur)
	for {
		select {
		case topo := <-c.Topology():
			if topo.Revision > afterRev {
				return topo
			}
		case <-deadline:
			t.Fatalf("no MsgTopologyChanged with revision > %d within %s", afterRev, dur)
			return nil
		}
	}
}

func topoTabIDs(topo *protocol.TopologyChanged) map[uint32]bool {
	out := make(map[uint32]bool, len(topo.Tabs))
	for _, ti := range topo.Tabs {
		out[ti.ID] = true
	}
	return out
}

// TestTopologyBroadcastCreateCloseMove verifies that structural
// changes one client makes (create tab, create window + move tab,
// close tab) are broadcast to OTHER attached clients of the session
// via MsgTopologyChanged.
func TestTopologyBroadcastCreateCloseMove(t *testing.T) {
	_, sockPath := startDaemon(t)

	a := mustDial(t, sockPath, "client-A")
	defer a.Close()
	b := mustDial(t, sockPath, "client-B")
	defer b.Close()

	<-a.Attached()
	attB := <-b.Attached()
	rev := attB.Revision

	// A drains everything; we only assert on B's topology stream.
	go drainClient(a, false, false)
	go drainClient(b, true, false) // keep B's topology for assertions

	if len(attB.Tabs) != 1 {
		t.Fatalf("expected 1 initial tab, got %d", len(attB.Tabs))
	}
	origTab := attB.Tabs[0].ID

	// 1. A creates a tab → B sees 2 tabs.
	if err := a.SendTabCreate(0, 80, 24, "", ""); err != nil {
		t.Fatalf("A SendTabCreate: %v", err)
	}
	topo := waitTopology(t, b, rev, 3*time.Second)
	rev = topo.Revision
	if len(topo.Tabs) != 2 {
		t.Fatalf("after A create, B sees %d tabs, want 2: %+v", len(topo.Tabs), topo.Tabs)
	}
	if !topoTabIDs(topo)[origTab] {
		t.Fatalf("after A create, original tab %d missing from B's view", origTab)
	}
	// Identify the new tab.
	var newTab uint32
	for id := range topoTabIDs(topo) {
		if id != origTab {
			newTab = id
		}
	}

	// 2. A creates a second window, then moves the new tab into it.
	if err := a.SendWindowCreate(0, 0, 100, 40); err != nil {
		t.Fatalf("A SendWindowCreate: %v", err)
	}
	topo = waitTopology(t, b, rev, 3*time.Second)
	rev = topo.Revision
	if len(topo.Windows) != 2 {
		t.Fatalf("after A window create, B sees %d windows, want 2", len(topo.Windows))
	}
	newWin := topo.Windows[1].ID

	if err := a.SendWindowMoveTab(newTab, newWin, 0); err != nil {
		t.Fatalf("A SendWindowMoveTab: %v", err)
	}
	topo = waitTopology(t, b, rev, 3*time.Second)
	rev = topo.Revision
	// The moved tab should now be in newWin's TabIDs.
	var moved bool
	for _, w := range topo.Windows {
		if w.ID == newWin {
			for _, id := range w.TabIDs {
				if id == newTab {
					moved = true
				}
			}
		}
	}
	if !moved {
		t.Fatalf("after A move, B doesn't show tab %d in window %d: %+v", newTab, newWin, topo.Windows)
	}

	// 3. A closes the new tab → B sees 1 tab.
	if err := a.SendTabClose(newTab); err != nil {
		t.Fatalf("A SendTabClose: %v", err)
	}
	topo = waitTopology(t, b, rev, 3*time.Second)
	if topoTabIDs(topo)[newTab] {
		t.Fatalf("after A close, tab %d still in B's view: %+v", newTab, topo.Tabs)
	}
	if !topoTabIDs(topo)[origTab] {
		t.Fatalf("after A close, original tab %d vanished from B's view", origTab)
	}
}

// TestScrollbackClearOnExitedTabNotifiesOthers verifies M2: a
// scrollback clear on a tab whose child has already exited still
// reaches OTHER attached clients (the publish loop stays alive for a
// held/exited tab to deliver the broadcast).
func TestScrollbackClearOnExitedTabNotifiesOthers(t *testing.T) {
	d, sockPath := startDaemon(t)

	a := mustDial(t, sockPath, "client-A")
	defer a.Close()
	b := mustDial(t, sockPath, "client-B")
	defer b.Close()

	attA := <-a.Attached()
	<-b.Attached()
	tabID := attA.Tabs[0].ID

	// A drains all; B drains all EXCEPT ScrollbackCleared (asserted).
	go drainClient(a, false, false)
	go drainClient(b, false, true)

	// Make the tab's child exit, then wait for it on the daemon side
	// (deterministic — the daemon's Tab.Exited closes on reap).
	sess := d.SessionByName("default")
	if sess == nil {
		t.Fatal("no default session")
	}
	tab := sess.Tab(tabID)
	if tab == nil {
		t.Fatalf("tab %d not found", tabID)
	}
	if err := a.SendInput(tabID, []byte("exit\r")); err != nil {
		t.Fatalf("A SendInput exit: %v", err)
	}
	select {
	case <-tab.Exited:
	case <-time.After(5 * time.Second):
		t.Fatal("tab child never exited")
	}
	// Give the publish loop a beat to ship MsgChildExit and settle.
	time.Sleep(100 * time.Millisecond)

	// Now clear scrollback on the exited tab from A.
	if err := a.SendClearScrollback(tabID); err != nil {
		t.Fatalf("A SendClearScrollback: %v", err)
	}

	// B must still be notified even though the tab's child is gone.
	select {
	case sc := <-b.ScrollbackCleared():
		if sc.ID != tabID {
			t.Fatalf("B got ScrollbackCleared for tab %d, want %d", sc.ID, tabID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client B never received ScrollbackCleared for the exited tab")
	}
}

// drainExceptCells consumes every client channel EXCEPT CellFull /
// CellDiff (which the caller reads to inspect tab content), so
// Client.Run never back-pressures.
func drainExceptCells(c *clientproto.Client) {
	for {
		select {
		case <-c.Closed():
			return
		case <-c.Cursor():
		case <-c.Title():
		case <-c.Bell():
		case <-c.ChildExit():
		case <-c.TabState():
		case <-c.ScrollbackAppend():
		case <-c.ScrollbackCleared():
		case <-c.TabCreated():
		case <-c.WindowCreated():
		case <-c.ClipboardSet():
		case <-c.ProposalsChanged():
		case <-c.Topology():
		case <-c.Errors():
		}
	}
}

// waitForMarkerOnTab reads cell frames for a SPECIFIC tab, maintaining
// a mirror, until needle appears or the timeout elapses. Frames for
// other tabs are ignored.
func waitForMarkerOnTab(t *testing.T, c *clientproto.Client, tabID uint32, needle string, dur time.Duration) bool {
	t.Helper()
	var mirror [][]protocol.Cell
	deadline := time.After(dur)
	for {
		for _, row := range mirror {
			var sb strings.Builder
			for _, cell := range row {
				if cell.Content == "" {
					sb.WriteByte(' ')
					continue
				}
				sb.WriteString(cell.Content)
			}
			if strings.Contains(sb.String(), needle) {
				return true
			}
		}
		select {
		case full := <-c.CellFull():
			if full.ID == tabID {
				mirror = full.Grid
			}
		case diff := <-c.CellDiff():
			if diff.ID == tabID {
				for _, e := range diff.Cells {
					if int(e.Row) < len(mirror) && int(e.Col) < len(mirror[e.Row]) {
						mirror[e.Row][e.Col] = e.Cell
					}
				}
			}
		case <-deadline:
			return false
		}
	}
}

// newestOtherTab waits until the session has a tab other than exclude
// and returns its ID.
func newestOtherTab(t *testing.T, sess *daemon.Session, exclude uint32, dur time.Duration) uint32 {
	t.Helper()
	deadline := time.After(dur)
	for {
		for _, tab := range sess.Tabs() {
			if tab.ID != exclude {
				return tab.ID
			}
		}
		select {
		case <-deadline:
			t.Fatal("no new tab appeared on the daemon")
			return 0
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestSecondClientSeesCreatedTabContent verifies HIGH 1: a tab created
// by client A is subscribed for client B too, so B receives B's own
// publish loop feeding cell content — not a blank Source.
func TestSecondClientSeesCreatedTabContent(t *testing.T) {
	d, sockPath := startDaemon(t)

	a := mustDial(t, sockPath, "client-A")
	defer a.Close()
	b := mustDial(t, sockPath, "client-B")
	defer b.Close()

	attA := <-a.Attached()
	<-b.Attached()
	origTab := attA.Tabs[0].ID

	go drainClient(a, false, false) // A fully drained; we assert on B
	go drainExceptCells(b)          // B: keep cell frames for the reader

	sess := d.SessionByName("default")
	if sess == nil {
		t.Fatal("no default session")
	}

	// A creates a new tab.
	if err := a.SendTabCreate(0, 80, 24, "", ""); err != nil {
		t.Fatalf("A SendTabCreate: %v", err)
	}
	newTab := newestOtherTab(t, sess, origTab, 3*time.Second)

	// A types into the NEW tab; B (a different client) must see the
	// output land — proving the daemon subscribed B to the tab A made.
	marker := "XEROTTY_SUBSCRIBE_ON_CREATE_OK"
	if err := a.SendInput(newTab, []byte("echo "+marker+"\r")); err != nil {
		t.Fatalf("A SendInput: %v", err)
	}
	if !waitForMarkerOnTab(t, b, newTab, marker, 5*time.Second) {
		t.Fatal("client B never saw content of the tab A created — daemon didn't subscribe B (blank tab)")
	}
}

// TestCloseTabUnsubscribesAllClients verifies HIGH 2: closing a tab
// tears down its publish loop on EVERY client, not just the closer —
// otherwise (with publish loops now outliving child exit) each viewer
// leaks a goroutine + terminal ref per created-then-closed tab.
func TestCloseTabUnsubscribesAllClients(t *testing.T) {
	d, sockPath := startDaemon(t)

	a := mustDial(t, sockPath, "client-A")
	defer a.Close()
	b := mustDial(t, sockPath, "client-B")
	defer b.Close()

	attA := <-a.Attached()
	<-b.Attached()
	origTab := attA.Tabs[0].ID

	go drainClient(a, false, false)
	go drainClient(b, false, false)

	sess := d.SessionByName("default")
	if sess == nil {
		t.Fatal("no default session")
	}

	// Warm up + settle so the baseline reflects steady state.
	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	base := runtime.NumGoroutine()

	const cycles = 15
	for i := 0; i < cycles; i++ {
		if err := a.SendTabCreate(0, 80, 24, "", ""); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		id := newestOtherTab(t, sess, origTab, 3*time.Second)
		if err := a.SendTabClose(id); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
		// Wait until the daemon dropped the tab.
		deadline := time.After(3 * time.Second)
		for sess.Tab(id) != nil {
			select {
			case <-deadline:
				t.Fatalf("tab %d never closed", id)
			case <-time.After(5 * time.Millisecond):
			}
		}
	}

	// After all the create/close churn the goroutine count must settle
	// back near baseline. A per-client subscription leak would leave
	// ~cycles publish loops (one per non-closing client) alive.
	settled := false
	for deadline := time.After(3 * time.Second); ; {
		runtime.GC()
		if n := runtime.NumGoroutine(); n <= base+8 {
			settled = true
			break
		}
		select {
		case <-deadline:
		case <-time.After(50 * time.Millisecond):
			continue
		}
		break
	}
	if !settled {
		runtime.GC()
		t.Fatalf("goroutines did not settle after %d create/close cycles: base=%d now=%d (subscription leak?)",
			cycles, base, runtime.NumGoroutine())
	}
}
