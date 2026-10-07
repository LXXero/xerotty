package app

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/daemonsource"
	"github.com/LXXero/xerotty/internal/testutil"
)

// TestPersistDaemonTabMoveTellsDaemon covers a cross-Window tab drag
// whose source is daemon-backed: the destination Window has no daemon
// window yet, so persistDaemonTabMove must mint one and send
// WindowMoveTab. Without it the daemon keeps the tab in the old
// window and a later reattach restores it there, not where the GUI
// showed it. A second attached client checks what the daemon recorded.
func TestPersistDaemonTabMoveTellsDaemon(t *testing.T) {
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
	cli.Hello("tabmove-gui")
	go cli.Run()
	cli.Attach("", false)
	<-cli.Attached()
	hub := daemonsource.NewHub(cli)
	defer hub.Stop()
	src, err := hub.NewTab(80, 24, "")
	if err != nil {
		t.Fatalf("new tab: %v", err)
	}

	obs, err := clientproto.Dial(sockPath)
	if err != nil {
		t.Fatalf("observer dial: %v", err)
	}
	defer obs.Close()
	obs.Hello("tabmove-observer")
	go obs.Run()
	obs.Attach("", false)
	att := <-obs.Attached()
	if len(att.Windows) != 1 {
		t.Fatalf("before move: daemon has %d windows, want 1", len(att.Windows))
	}
	oldWin := att.Windows[0].ID

	a := &App{daemonHub: hub}
	w := &Window{app: a, width: 800, height: 600}
	w.persistDaemonTabMove(src)

	if w.daemonWindowID == 0 || w.daemonWindowID == oldWin {
		t.Fatalf("daemonWindowID = %d, want a new daemon window (old %d)", w.daemonWindowID, oldWin)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case topo := <-obs.Topology():
			for _, win := range topo.Windows {
				if win.ID != w.daemonWindowID {
					continue
				}
				for _, id := range win.TabIDs {
					if id == src.TabID() {
						return
					}
				}
			}
		case <-deadline:
			t.Fatalf("daemon never moved tab %d into window %d", src.TabID(), w.daemonWindowID)
		}
	}
}
