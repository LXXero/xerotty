package daemon_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/protocol"
)

// TestDaemonRoundTrip is the Phase 0 end-to-end acceptance test:
// spawn xerottyd in-process on a temp unix socket, attach a client,
// send "echo HELLO\r" through a tab's PTY, wait for a CellFull
// frame that contains the string "HELLO" in the grid. Proves the
// full stack works without needing a UI.
//
// Skipped automatically if the test runs in an environment without
// PTY support (CI sometimes blocks /dev/ptmx).
func TestDaemonRoundTrip(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()

	d := daemon.New(&cfg, sockPath)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() {
		_ = d.Stop()
		<-doneRun
	}()
	// Give the listener a moment to bind.
	time.Sleep(50 * time.Millisecond)

	c, err := clientproto.Dial(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if _, err := c.Hello("test-client"); err != nil {
		t.Fatalf("hello: %v", err)
	}

	go c.Run()

	if err := c.Attach("", true); err != nil {
		t.Fatalf("attach: %v", err)
	}

	// Wait for the Attached response. Must time out aggressively
	// so a daemon bug doesn't hang the test suite forever.
	var attached *protocol.Attached
	select {
	case attached = <-c.Attached():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Attached")
	}
	if len(attached.Tabs) != 1 {
		t.Fatalf("expected 1 tab, got %d", len(attached.Tabs))
	}
	tabID := attached.Tabs[0].ID

	// Daemon sends CellFull on first paint then CellDiff for
	// subsequent updates. Maintain a local mirror so we can check
	// the grid regardless of which frame type the daemon emits.
	var mirror [][]protocol.Cell

	// Drain initial CellFull (from attach's initial paint).
	select {
	case full := <-c.CellFull():
		mirror = full.Grid
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial CellFull")
	}

	// Echo something distinctive through the PTY. Use a marker
	// that's unlikely to collide with shell prompt text.
	marker := "XEROTTY_PHASE0_OK"
	if err := c.SendInput(tabID, []byte("echo "+marker+"\r")); err != nil {
		t.Fatalf("send input: %v", err)
	}

	// Wait up to 5s for the marker to appear in our mirror after
	// applying any CellFull / CellDiff frames the daemon sends.
	deadline := time.After(5 * time.Second)
	for {
		if mirrorContains(mirror, marker) {
			return // success
		}
		select {
		case full := <-c.CellFull():
			mirror = full.Grid
		case diff := <-c.CellDiff():
			for _, e := range diff.Cells {
				if int(e.Row) < len(mirror) && int(e.Col) < len(mirror[e.Row]) {
					mirror[e.Row][e.Col] = e.Cell
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for marker %q in cell grid", marker)
		}
	}
}

// mirrorContains returns true if any row of the cell mirror,
// reconstructed left-to-right, contains needle.
func mirrorContains(mirror [][]protocol.Cell, needle string) bool {
	for _, row := range mirror {
		var sb strings.Builder
		for _, c := range row {
			if c.Content == "" {
				sb.WriteByte(' ')
				continue
			}
			sb.WriteString(c.Content)
		}
		if strings.Contains(sb.String(), needle) {
			return true
		}
	}
	return false
}

// TestDaemonWindowLayout exercises the Window concept: open multiple
// tabs across two windows, detach, reattach, confirm the layout
// survives. This is the "roam between machines, get my layout back"
// promise made concrete.
func TestDaemonWindowLayout(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()

	d := daemon.New(&cfg, sockPath)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() {
		_ = d.Stop()
		<-doneRun
	}()
	time.Sleep(50 * time.Millisecond)

	// First client: create a second window + populate both with tabs.
	c1, err := clientproto.Dial(sockPath)
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	if _, err := c1.Hello("client-1"); err != nil {
		t.Fatalf("hello 1: %v", err)
	}
	go c1.Run()
	if err := c1.Attach("", true); err != nil {
		t.Fatalf("attach 1: %v", err)
	}
	attached1 := <-c1.Attached()
	if len(attached1.Windows) != 1 {
		t.Fatalf("first attach: expected 1 window, got %d", len(attached1.Windows))
	}
	w1ID := attached1.Windows[0].ID
	tab1ID := attached1.Tabs[0].ID

	// Create a second window.
	if err := c1.SendWindowCreate(100, 100, 800, 600); err != nil {
		t.Fatalf("send WindowCreate: %v", err)
	}
	created := <-c1.WindowCreated()
	w2ID := created.Info.ID

	// Add a tab in each window.
	if err := c1.SendTabCreate(w1ID, 80, 24, "", ""); err != nil {
		t.Fatalf("tabcreate w1: %v", err)
	}
	tc1 := <-c1.TabCreated()
	if tc1.WindowID != w1ID {
		t.Errorf("tab landed in window %d, want %d", tc1.WindowID, w1ID)
	}
	if err := c1.SendTabCreate(w2ID, 80, 24, "", ""); err != nil {
		t.Fatalf("tabcreate w2: %v", err)
	}
	tc2 := <-c1.TabCreated()
	if tc2.WindowID != w2ID {
		t.Errorf("second tab landed in window %d, want %d", tc2.WindowID, w2ID)
	}

	// Detach + reattach (close c1, open c2) and check the layout
	// matches what we set up.
	c1.Close()
	<-c1.Closed()
	time.Sleep(20 * time.Millisecond)

	c2, err := clientproto.Dial(sockPath)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer c2.Close()
	if _, err := c2.Hello("client-2"); err != nil {
		t.Fatalf("hello 2: %v", err)
	}
	go c2.Run()
	if err := c2.Attach("", false); err != nil {
		t.Fatalf("attach 2: %v", err)
	}
	attached2 := <-c2.Attached()
	if len(attached2.Windows) != 2 {
		t.Fatalf("reattach: expected 2 windows, got %d", len(attached2.Windows))
	}
	// Tab counts per window: w1 has the initial tab + the one we
	// added = 2; w2 has 1.
	for _, w := range attached2.Windows {
		switch w.ID {
		case w1ID:
			if len(w.TabIDs) != 2 {
				t.Errorf("window 1: %d tabs, want 2", len(w.TabIDs))
			}
			if w.TabIDs[0] != tab1ID {
				t.Errorf("window 1: first tab %d, want %d", w.TabIDs[0], tab1ID)
			}
		case w2ID:
			if len(w.TabIDs) != 1 {
				t.Errorf("window 2: %d tabs, want 1", len(w.TabIDs))
			}
		default:
			t.Errorf("unexpected window ID %d", w.ID)
		}
	}
	if len(attached2.Tabs) != 3 {
		t.Errorf("total tabs: %d, want 3", len(attached2.Tabs))
	}
}

