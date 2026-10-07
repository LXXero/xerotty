package daemon_test

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/protocol"
	"github.com/LXXero/xerotty/internal/testutil"
)

// attachTo dials, attaches without creating tabs, and drains frames.
func attachTo(t *testing.T, sock, id string) (*clientproto.Client, *protocol.Attached) {
	t.Helper()
	c, err := clientproto.Dial(sock)
	if err != nil {
		t.Fatalf("dial %s: %v", id, err)
	}
	if _, err := c.Hello(id); err != nil {
		t.Fatalf("hello %s: %v", id, err)
	}
	go c.Run()
	if err := c.Attach("", false); err != nil {
		t.Fatalf("attach %s: %v", id, err)
	}
	var att *protocol.Attached
	select {
	case att = <-c.Attached():
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never attached", id)
	}
	go drainClient(c, false, false)
	t.Cleanup(func() { c.Close() })
	return c, att
}

func waitGrid(t *testing.T, sess *daemon.Session, tabID uint32, cols, rows int, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		tab := sess.Tab(tabID)
		if tab != nil && tab.Term.Width() == cols && tab.Term.Height() == rows {
			return
		}
		if time.Now().After(deadline) {
			w, h := -1, -1
			if tab != nil {
				w, h = tab.Term.Width(), tab.Term.Height()
			}
			t.Fatalf("%s: tab %d grid %dx%d, want %dx%d", msg, tabID, w, h, cols, rows)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func holdGrid(t *testing.T, sess *daemon.Session, tabID uint32, cols, rows int, msg string) {
	t.Helper()
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		tab := sess.Tab(tabID)
		if tab == nil || tab.Term.Width() != cols || tab.Term.Height() != rows {
			w, h := -1, -1
			if tab != nil {
				w, h = tab.Term.Width(), tab.Term.Height()
			}
			t.Fatalf("%s: tab %d grid %dx%d, want it held at %dx%d", msg, tabID, w, h, cols, rows)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSizeOwnerSurvivesResume: the client that owned a shared tab's
// grid keeps it across a daemon upgrade. Ownership lived only in the
// connections, so after every restart the first client to reconnect
// (the local GUI, with no ssh hop) claimed each tab with its own
// size, and the remote GUI that was actually using the tab saw it
// letterboxed with the bottom rows cut off until it resized its
// window. A tab nobody had sized is still claimed by the first
// report, as before.
func TestSizeOwnerSurvivesResume(t *testing.T) {
	d, sock := startDaemon(t)
	small := mustDial(t, sock, "small") // creates the first tab
	go drainClient(small, false, false)
	// The session and its first tab appear once the attach lands.
	var sess *daemon.Session
	var tabID uint32
	deadline := time.Now().Add(5 * time.Second)
	for tabID == 0 && time.Now().Before(deadline) {
		if sess = d.SessionByName("default"); sess != nil {
			if tabs := sess.Tabs(); len(tabs) > 0 {
				tabID = tabs[0].ID
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if tabID == 0 {
		t.Fatal("no tab")
	}
	// A second tab no client ever sizes: the control case.
	unowned, _, err := sess.NewTab(0, 80, 24, "", "", nil)
	if err != nil {
		t.Skipf("no PTY: %v", err)
	}

	if err := small.SendResize(tabID, 100, 30); err != nil {
		t.Fatal(err)
	}
	waitGrid(t, sess, tabID, 100, 30, "small's first claim")
	big, _ := attachTo(t, sock, "big")
	// A first report seeds while small owns; a changed one is a real
	// resize and claims.
	_ = big.SendResize(tabID, 150, 40)
	holdGrid(t, sess, tabID, 100, 30, "big's seeding report must not claim")
	_ = big.SendResize(tabID, 160, 50)
	waitGrid(t, sess, tabID, 160, 50, "big's resize")

	// ---- Upgrade: serialize, resume in a fresh daemon on another socket.
	d.DisconnectClients()
	_ = d.Stop()
	st, keep, err := d.SerializeUpgrade()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	t.Cleanup(func() {
		for _, ts := range st.Tabs {
			_ = syscall.Kill(ts.ChildPID, syscall.SIGKILL)
		}
		for _, f := range keep {
			_ = f.Close()
		}
	})
	var owned, control *struct{ ok bool }
	for _, ts := range st.Tabs {
		switch ts.ID {
		case tabID:
			owned = &struct{ ok bool }{ts.SizeOwned}
		case unowned.ID:
			control = &struct{ ok bool }{ts.SizeOwned}
		}
	}
	if owned == nil || !owned.ok {
		t.Fatalf("handoff does not record tab %d as size-owned: %+v", tabID, owned)
	}
	if control == nil || control.ok {
		t.Fatalf("handoff records the never-sized tab %d as owned: %+v", unowned.ID, control)
	}

	cfg := config.Default()
	cfg.Shell = "/bin/sh"
	sock2 := filepath.Join(testutil.SockDir(t), "resumed.sock")
	d2 := daemon.New(&cfg, sock2)
	if err := d2.ResumeFromHandoff(st); err != nil {
		t.Fatalf("resume: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- d2.Run() }()
	t.Cleanup(func() { _ = d2.Stop(); <-done })
	time.Sleep(50 * time.Millisecond)
	sess2 := d2.SessionByName("default")
	waitGrid(t, sess2, tabID, 160, 50, "resumed grid")

	// The small client reconnects first (it always does: no ssh hop)
	// and reports its size. That must not take the grid from big.
	small2, _ := attachTo(t, sock2, "small")
	_ = small2.SendResize(tabID, 100, 30)
	holdGrid(t, sess2, tabID, 160, 50, "first reconnect must not claim an owned tab")
	// The never-sized tab is claimed by that same report, as before.
	_ = small2.SendResize(unowned.ID, 100, 30)
	waitGrid(t, sess2, unowned.ID, 100, 30, "unowned tab follows the first report")

	big2, _ := attachTo(t, sock2, "big")
	_ = big2.SendResize(tabID, 160, 50)
	holdGrid(t, sess2, tabID, 160, 50, "owner's reconnect")

	// Typing on the small client is a real claim: the grid follows it.
	if err := small2.SendInput(tabID, []byte("x")); err != nil {
		t.Fatal(err)
	}
	waitGrid(t, sess2, tabID, 100, 30, "typing claims the grid")
}
