package app

import (
	"math"
	"path/filepath"
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

// attachHub dials the daemon as a named GUI client and wraps it in a
// Hub whose topology snapshots are queued on the returned channel, so
// the test goroutine can reconcile them the way the frame loop does
// (main thread, not the router goroutine).
func attachHub(t *testing.T, sockPath, name string) (*daemonsource.Hub, chan *protocol.TopologyChanged) {
	t.Helper()
	cli, err := clientproto.Dial(sockPath)
	if err != nil {
		t.Fatalf("dial %s: %v", name, err)
	}
	t.Cleanup(func() { cli.Close() })
	cli.Hello(name)
	go cli.Run()
	cli.Attach("", false)
	<-cli.Attached()
	hub := daemonsource.NewHub(cli)
	t.Cleanup(hub.Stop)
	topo := make(chan *protocol.TopologyChanged, 16)
	hub.SetTopologyCallback(func(s *protocol.TopologyChanged) { topo <- s })
	return hub, topo
}

// gridWindow builds a Window whose gridSize() is exactly cols×rows,
// using the same geometry formula spawnWindowImpl does, with a daemon
// window of its own on a's hub (the spawnWindowImpl daemon branch).
func gridWindow(t *testing.T, a *App, cols, rows int) *Window {
	t.Helper()
	w := &Window{app: a, cellW: 8, cellH: 16}
	pad := float32(a.cfg.Appearance.Padding) * 2
	w.width = int(math.Ceil(float64(float32(cols)*w.cellW + pad + cellSafetyMarginH + cellOriginInsetX)))
	w.height = int(math.Ceil(float64(float32(rows)*w.cellH + pad + cellSafetyMarginV)))
	if c, r := w.gridSize(); c != cols || r != rows {
		t.Fatalf("gridWindow: gridSize = %dx%d, want %dx%d", c, r, cols, rows)
	}
	w.tabs = tabs.NewManager(&a.cfg)
	a.installSourceFactory(w)
	id, err := a.daemonHub.CreateWindow(0, 0, int32(w.width), int32(w.height))
	if err != nil {
		t.Fatalf("create daemon window: %v", err)
	}
	w.daemonWindowID = id
	a.daemonHub.SetDefaultWindowID(id)
	return w
}

// reconcileUntil drains topology snapshots into reconcileDaemonTabs
// until one mentions tabID.
func reconcileUntil(t *testing.T, a *App, topo chan *protocol.TopologyChanged, tabID uint32) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case s := <-topo:
			a.reconcileDaemonTabs(a.daemonHub, "", s)
			for _, ti := range s.Tabs {
				if ti.ID == tabID {
					return
				}
			}
		case <-deadline:
			t.Fatalf("no topology snapshot listing tab %d", tabID)
		}
	}
}

// windowShowsTab reports whether w has a GUI tab for daemon tab id.
func windowShowsTab(w *Window, id uint32) bool {
	for _, tab := range w.tabs.Tabs {
		if ds, ok := tab.Terminal.(*daemonsource.Source); ok && ds.TabID() == id {
			return true
		}
	}
	return false
}

// TestNewWindowTabKeepsItsOwnGrid covers `xerotty -e mutt` opening a
// new window while larger windows exist: the new window's tab must stay
// at the NEW window's grid. A second GUI attached to the same daemon
// (another machine over SSH) mirrors the tab into its own larger window
// and reports that window's grid. The creating window never sent a
// Resize (the tab was minted at exactly its grid), so before the fix
// that mirror's first size report claimed the tab and the daemon
// resized the PTY to the other machine's 152x57 — mutt drew past the
// bottom of the 80x24 window that showed it.
func TestNewWindowTabKeepsItsOwnGrid(t *testing.T) {
	sockPath := filepath.Join(testutil.SockDir(t), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	// Local GUI: a large existing window with a tab, then a new
	// configured-size window (spawnWindowImpl's daemon branch).
	hubA, topoA := attachHub(t, sockPath, "xerotty-gui")
	a := &App{cfg: config.Default(), daemonHub: hubA}
	big := gridWindow(t, a, 152, 57)
	a.windows = append(a.windows, big)
	if _, err := big.tabs.NewTab(152, 57, ""); err != nil {
		t.Fatalf("big window tab: %v", err)
	}

	// Remote GUI: one large window already showing the session.
	hubB, topoB := attachHub(t, sockPath, "xerotty-gui:remote")
	b := &App{cfg: config.Default(), daemonHub: hubB}
	remote := gridWindow(t, b, 152, 57)
	b.windows = append(b.windows, remote)
	if _, err := remote.tabs.NewTab(152, 57, ""); err != nil {
		t.Fatalf("remote window tab: %v", err)
	}

	small := gridWindow(t, a, 80, 24)
	tab, err := small.tabs.NewTabCmd(80, 24, "", nil)
	if err != nil {
		t.Fatalf("new window tab: %v", err)
	}
	a.windows = append(a.windows, small)
	src := tab.Terminal.(*daemonsource.Source)
	id := src.TabID()

	// Both GUIs reconcile the broadcast that announced the tab. The
	// remote one adopts it into its window and resizes it to 152x57.
	reconcileUntil(t, a, topoA, id)
	reconcileUntil(t, b, topoB, id)
	if windowShowsTab(big, id) {
		t.Fatalf("new window's tab %d was also adopted into the big window", id)
	}
	if !windowShowsTab(small, id) || len(small.tabs.Tabs) != 1 {
		t.Fatalf("new window should show exactly its own tab %d (has %d tabs)", id, len(small.tabs.Tabs))
	}
	// A round trip on the remote connection guarantees the daemon has
	// handled the remote's Resize (one connection's frames are
	// processed in order).
	if _, err := hubB.CreateWindow(0, 0, 100, 100); err != nil {
		t.Fatalf("remote round trip: %v", err)
	}

	// The local tab's grid only changes when the daemon resizes the
	// PTY and republishes; watch long enough for that to land.
	want := [2]int{80, 24}
	if c, r := small.gridSize(); c != want[0] || r != want[1] {
		t.Fatalf("small.gridSize = %dx%d", c, r)
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := [2]int{src.Width(), src.Height()}; got != want {
			t.Fatalf("new window's tab PTY resized to %dx%d, want its window's grid %dx%d", got[0], got[1], want[0], want[1])
		}
		time.Sleep(20 * time.Millisecond)
	}
}
