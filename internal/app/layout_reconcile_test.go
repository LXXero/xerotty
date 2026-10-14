package app

import (
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/daemonsource"
	"github.com/LXXero/xerotty/internal/protocol"
	"github.com/LXXero/xerotty/internal/tabs"
	"github.com/LXXero/xerotty/internal/testutil"
)

// A tab the GUI doesn't show (another GUI's) is never moved; the GUI's
// own tabs are placed ahead of it.
func TestPlanLayoutLeavesForeignTabs(t *testing.T) {
	daemon := []protocol.WindowInfo{{ID: 1, TabIDs: []uint32{9, 1}, FocusedTabID: 9}}
	gui := []guiLayoutWindow{{daemonID: 1, tabs: []uint32{1}, active: 1}}
	ops := planLayout(gui, daemon, nil)
	for _, op := range ops {
		if op.kind == opMoveTab && op.tab == 9 {
			t.Fatalf("moved a tab the GUI doesn't show: %+v", ops)
		}
	}
	if len(ops) == 0 {
		t.Fatalf("expected the GUI's tab to be moved ahead of the foreign one")
	}
}

// An already-matching layout needs no corrections, and an empty daemon
// window this GUI never owned is left alone.
func TestPlanLayoutConverged(t *testing.T) {
	daemon := []protocol.WindowInfo{
		{ID: 1, TabIDs: []uint32{1, 2}, FocusedTabID: 2},
		{ID: 2},
	}
	gui := []guiLayoutWindow{{daemonID: 1, tabs: []uint32{1, 2}, active: 2}}
	if ops := planLayout(gui, daemon, map[uint32]bool{1: true}); len(ops) != 0 {
		t.Fatalf("converged layout produced ops: %+v", ops)
	}
}

func attachSnapshot(t *testing.T, sockPath string) *protocol.Attached {
	t.Helper()
	cli, err := clientproto.Dial(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()
	cli.Hello("snapshot")
	go cli.Run()
	cli.Attach("", false)
	return <-cli.Attached()
}

func asTopology(at *protocol.Attached) *protocol.TopologyChanged {
	return &protocol.TopologyChanged{Revision: at.Revision, Windows: at.Windows, Tabs: at.Tabs}
}

// TestReconcileConvergesDaemonToGUILayout: the daemon's record is
// deliberately wrong in every way drift has been seen: a tab in the
// wrong daemon window, a tab only in the default window, a GUI window
// whose daemon window no longer exists, and an extra daemon window.
// After one reconcile the daemon's record, and so the layout a GUI
// restart rebuilds, is the GUI's.
func TestReconcileConvergesDaemonToGUILayout(t *testing.T) {
	sockPath := filepath.Join(testutil.SockDir(t), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	cli, err := clientproto.Dial(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()
	cli.Hello("reconcile-gui")
	go cli.Run()
	cli.Attach("", false)
	<-cli.Attached()
	hub := daemonsource.NewHub(cli)
	defer hub.Stop()

	var s []*daemonsource.Source
	for i := 0; i < 4; i++ {
		src, err := hub.NewTab(80, 24, "")
		if err != nil {
			t.Fatalf("new tab: %v", err)
		}
		s = append(s, src)
	}
	a, b, c, dd := s[0], s[1], s[2], s[3]
	defaultWin := attachSnapshot(t, sockPath).Windows[0].ID

	// Extra daemon window holding B.
	extra, err := hub.CreateWindow(0, 0, 800, 600)
	if err != nil {
		t.Fatalf("create window: %v", err)
	}
	_ = cli.SendWindowMoveTab(b.TabID(), extra, -1)
	deadline := time.Now().Add(3 * time.Second)
	for len(attachSnapshot(t, sockPath).Windows[1].TabIDs) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("daemon never moved B into the extra window")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// GUI: window 1 = A B D (active B); window 2 = C, mapped to a
	// daemon window that doesn't exist.
	app := &App{daemonHub: hub}
	w1 := &Window{app: app, width: 800, height: 600, daemonWindowID: defaultWin}
	w2 := &Window{app: app, width: 800, height: 600}
	w2.daemonWindowIDs = map[*daemonsource.Hub]uint32{hub: 777}
	w1.tabs = &tabs.Manager{Tabs: []*tabs.Tab{{Terminal: a}, {Terminal: b}, {Terminal: dd}}, ActiveIdx: 1}
	w2.tabs = &tabs.Manager{Tabs: []*tabs.Tab{{Terminal: c}}}
	app.windows = []*Window{w1, w2}

	topo := asTopology(attachSnapshot(t, sockPath))
	if n := app.reconcileDaemonLayout(hub, "local", topo); n == 0 {
		t.Fatal("reconcile found no drift in a deliberately drifted layout")
	}

	want := [][]uint32{
		{a.TabID(), b.TabID(), dd.TabID()},
		{c.TabID()},
	}
	deadline = time.Now().Add(3 * time.Second)
	var got [][]uint32
	var focus []uint32
	for {
		got, focus = restoredLayout(t, sockPath)
		if reflect.DeepEqual(got, want) && daemonWindowCount(t, sockPath) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restored layout = %v, want %v (daemon windows %d, want 2)", got, want, daemonWindowCount(t, sockPath))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if focus[0] != b.TabID() {
		t.Errorf("window 1 active tab = %d, want B (%d)", focus[0], b.TabID())
	}

	// A second pass against the converged record has nothing to fix.
	topo = asTopology(attachSnapshot(t, sockPath))
	if n := app.reconcileDaemonLayout(hub, "local", topo); n != 0 {
		t.Errorf("second reconcile sent %d corrections, want 0", n)
	}

	// list_tabs shows where each tab sits.
	app.noteTopology(hub, topo)
	app.publishTabLayout()
	idx := map[uint32]int{}
	for _, r := range app.ListTabs() {
		if r.WindowIndex >= 0 {
			idx[parseTabID(t, r.NSID)] = r.WindowIndex
			if r.DaemonWindowID == 0 || r.DaemonWindowID != r.GUIDaemonWindowID {
				t.Errorf("tab %s: daemon window %d vs GUI window's %d", r.NSID, r.DaemonWindowID, r.GUIDaemonWindowID)
			}
		}
	}
	if idx[a.TabID()] != 0 || idx[c.TabID()] != 1 {
		t.Errorf("window index per tab = %v", idx)
	}
}

func parseTabID(t *testing.T, nsID string) uint32 {
	t.Helper()
	n, err := strconv.ParseUint(nsID[strings.LastIndex(nsID, ":")+1:], 10, 32)
	if err != nil {
		t.Fatalf("bad tab id %q: %v", nsID, err)
	}
	return uint32(n)
}
