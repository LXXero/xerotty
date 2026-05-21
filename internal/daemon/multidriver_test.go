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

// TestMultiDriverBroadcast verifies that when one attached client
// sends input to a tab, the resulting PTY output reaches BOTH
// attached clients. This is the central guarantee of multi-attach:
// multiple drivers ↔ one PTY ↔ broadcast updates.
//
// Both clients see the marker because publishLoop runs per
// (client, tab) and PTY DataCh fans out to every subscriber.
func TestMultiDriverBroadcast(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)

	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	c1 := mustDial(t, sockPath, "driver-1")
	defer c1.Close()
	c2 := mustDial(t, sockPath, "driver-2")
	defer c2.Close()

	attached1 := <-c1.Attached()
	attached2 := <-c2.Attached()
	if attached1.Tabs[0].ID != attached2.Tabs[0].ID {
		t.Fatalf("clients see different tab IDs: %d vs %d",
			attached1.Tabs[0].ID, attached2.Tabs[0].ID)
	}
	tabID := attached1.Tabs[0].ID

	// Drain both clients' initial CellFull (the first frame each
	// gets after attach).
	mirror1 := drainInitialFrame(t, c1, "c1")
	mirror2 := drainInitialFrame(t, c2, "c2")

	// Driver 1 sends; both should see it land.
	marker := "XEROTTY_MULTIDRIVER_OK"
	if err := c1.SendInput(tabID, []byte("echo "+marker+"\r")); err != nil {
		t.Fatalf("send input: %v", err)
	}

	// Race both clients toward the marker. Each gets its own
	// subscription so the daemon publishes to both independently.
	got1 := waitForMarker(t, c1, &mirror1, marker, 5*time.Second)
	got2 := waitForMarker(t, c2, &mirror2, marker, 5*time.Second)
	if !got1 {
		t.Error("driver-1 (the sender) didn't see the marker")
	}
	if !got2 {
		t.Error("driver-2 (the observer) didn't see the marker")
	}

	// Verify attached-clients listing reflects both connections.
	clients := d.AttachedClients()
	if len(clients) != 2 {
		t.Fatalf("expected 2 attached clients, got %d", len(clients))
	}
	ids := make(map[string]bool)
	for _, ac := range clients {
		ids[ac.ClientID] = true
	}
	if !ids["driver-1"] || !ids["driver-2"] {
		t.Errorf("attached clients didn't report expected IDs: %+v", clients)
	}
}

// TestHostnameInHelloAck confirms the daemon advertises its hostname
// so UIs can render host badges.
func TestHostnameInHelloAck(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	c, err := clientproto.Dial(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	ack, err := c.Hello("host-test")
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	if ack.Hostname == "" {
		t.Error("expected non-empty Hostname in HelloAck")
	}
	if ack.ServerID == "" || !strings.Contains(ack.ServerID, ":") {
		t.Errorf("expected ServerID to be hostname:pid, got %q", ack.ServerID)
	}
}

// --- helpers shared with multi-driver tests ---

func mustDial(t *testing.T, sockPath, clientID string) *clientproto.Client {
	t.Helper()
	c, err := clientproto.Dial(sockPath)
	if err != nil {
		t.Fatalf("dial %s: %v", clientID, err)
	}
	if _, err := c.Hello(clientID); err != nil {
		t.Fatalf("hello %s: %v", clientID, err)
	}
	go c.Run()
	if err := c.Attach("", true); err != nil {
		t.Fatalf("attach %s: %v", clientID, err)
	}
	return c
}

func drainInitialFrame(t *testing.T, c *clientproto.Client, who string) [][]protocol.Cell {
	t.Helper()
	select {
	case full := <-c.CellFull():
		return full.Grid
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: no initial CellFull", who)
		return nil
	}
}

func waitForMarker(t *testing.T, c *clientproto.Client, mirror *[][]protocol.Cell, needle string, dur time.Duration) bool {
	t.Helper()
	deadline := time.After(dur)
	for {
		for _, row := range *mirror {
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
		select {
		case full := <-c.CellFull():
			*mirror = full.Grid
		case diff := <-c.CellDiff():
			for _, e := range diff.Cells {
				if int(e.Row) < len(*mirror) && int(e.Col) < len((*mirror)[e.Row]) {
					(*mirror)[e.Row][e.Col] = e.Cell
				}
			}
		case <-c.Cursor():
			// Cursor updates don't change cells; ignore.
		case <-deadline:
			return false
		}
	}
}
