package app

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/daemonsource"
	"github.com/LXXero/xerotty/internal/tabs"
	"github.com/LXXero/xerotty/internal/testutil"
)

// restoredLayout rebuilds the window layout the way a GUI restart does:
// a fresh client attaches and daemonAdoptSnapshots turns the attach
// snapshot into one GUI window per daemon window. Each entry is that
// window's tab IDs in order.
func restoredLayout(t *testing.T, sockPath string) ([][]uint32, []uint32) {
	t.Helper()
	cli, err := clientproto.Dial(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()
	cli.Hello("restore-check")
	go cli.Run()
	cli.Attach("", false)
	att := <-cli.Attached()
	var layout [][]uint32
	var focus []uint32
	for _, snap := range daemonAdoptSnapshots(att) {
		var ids []uint32
		for _, ts := range snap.Tabs {
			ids = append(ids, ts.ID)
		}
		layout = append(layout, ids)
		focus = append(focus, snap.FocusedTabID)
	}
	return layout, focus
}

func daemonWindowCount(t *testing.T, sockPath string) int {
	t.Helper()
	cli, err := clientproto.Dial(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()
	cli.Hello("window-count")
	go cli.Run()
	cli.Attach("", false)
	return len((<-cli.Attached()).Windows)
}

// TestWindowLayoutSurvivesTabShuffling: drag a tab to another window,
// tear a tab off into a new window and drag it back, and let the
// emptied window go. After a restart the daemon must hand back the same
// windows with the same tab order that the GUI was showing.
func TestWindowLayoutSurvivesTabShuffling(t *testing.T) {
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
	cli.Hello("layout-gui")
	go cli.Run()
	cli.Attach("", false)
	att := <-cli.Attached()
	hub := daemonsource.NewHub(cli)
	defer hub.Stop()

	var srcs []*daemonsource.Source
	for i := 0; i < 4; i++ {
		s, err := hub.NewTab(80, 24, "")
		if err != nil {
			t.Fatalf("new tab: %v", err)
		}
		srcs = append(srcs, s)
	}
	a, b, c, dd := srcs[0], srcs[1], srcs[2], srcs[3]
	_ = att

	app := &App{daemonHub: hub}
	// GUI window 1 is the one the daemon created first.
	cli2, _ := clientproto.Dial(sockPath)
	defer cli2.Close()
	cli2.Hello("probe")
	go cli2.Run()
	cli2.Attach("", false)
	probe := <-cli2.Attached()
	if len(probe.Windows) != 1 {
		t.Fatalf("daemon has %d windows before shuffling, want 1", len(probe.Windows))
	}
	w1 := &Window{app: app, width: 800, height: 600, daemonWindowID: probe.Windows[0].ID}
	w2 := &Window{app: app, width: 800, height: 600}
	w3 := &Window{app: app, width: 800, height: 600}
	_ = a
	_ = b

	// Drag C to window 2.
	w2.persistDaemonTabMove(c)
	// Tear D off into window 3, then drag it back to window 1.
	w3.persistDaemonTabMove(dd)
	w1.persistDaemonTabMove(dd)
	// Window 3 is now empty; the GUI reaps it.
	w3.tabs = &tabs.Manager{}
	w3.pendingClose = true
	w1.tabs = &tabs.Manager{Tabs: []*tabs.Tab{{Terminal: a}, {Terminal: b}, {Terminal: dd}}}
	w2.tabs = &tabs.Manager{Tabs: []*tabs.Tab{{Terminal: c}}}
	app.windows = []*Window{w1, w2, w3}
	app.reapClosedWindows()

	// What the GUI shows: window 1 = A B D, window 2 = C.
	want := [][]uint32{
		{a.TabID(), b.TabID(), dd.TabID()},
		{c.TabID()},
	}
	got, _ := restoredLayout(t, sockPath)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("restored layout = %v, want %v", got, want)
	}
	// The emptied window must be gone from the daemon, not left
	// registered with no tabs.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if n := daemonWindowCount(t, sockPath); n == 2 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("daemon still has %d windows, want 2 (emptied window not closed)", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
