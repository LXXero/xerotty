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

	// Drain initial CellFull (from attach's initial paint).
	select {
	case <-c.CellFull():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial CellFull")
	}

	// Echo something distinctive through the PTY. Use a marker
	// that's unlikely to collide with shell prompt text.
	marker := "XEROTTY_PHASE0_OK"
	if err := c.SendInput(tabID, []byte("echo "+marker+"\r")); err != nil {
		t.Fatalf("send input: %v", err)
	}

	// Wait up to 5s for a CellFull whose grid contains the marker.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case full := <-c.CellFull():
			if gridContains(full, marker) {
				return // success
			}
		case <-deadline:
			t.Fatalf("timed out waiting for marker %q in cell grid", marker)
		}
	}
}

// gridContains returns true if any row of the CellFull's grid,
// reconstructed left-to-right, contains the given substring.
func gridContains(f *protocol.CellFull, needle string) bool {
	for _, row := range f.Grid {
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
