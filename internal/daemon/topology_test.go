package daemon_test

import (
	"path/filepath"
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
