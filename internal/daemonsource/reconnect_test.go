package daemonsource_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/daemonsource"
	"github.com/LXXero/xerotty/internal/protocol"
)

// TestHubReconnectResyncsSources is the layer-4b regression: when the
// daemon connection drops, a Hub with a redial dialer must re-dial in
// place, keep the SAME Source objects (identity stable), clear their
// reconnecting flag, and route input/output over the fresh connection —
// all without the caller doing anything. The daemon-side session
// persists across the drop (same process), so the tab survives.
func TestHubReconnectResyncsSources(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	dial := func() (*clientproto.Client, *protocol.Attached, error) {
		c, err := clientproto.Dial(sockPath)
		if err != nil {
			return nil, nil, err
		}
		if _, err := c.Hello("reconnect"); err != nil {
			_ = c.Close()
			return nil, nil, err
		}
		go c.Run()
		if err := c.Attach("", false); err != nil {
			_ = c.Close()
			return nil, nil, err
		}
		select {
		case a := <-c.Attached():
			return c, a, nil
		case <-time.After(2 * time.Second):
			_ = c.Close()
			return nil, nil, fmt.Errorf("no attached")
		}
	}

	cli, _, err := dial()
	if err != nil {
		t.Fatalf("initial dial: %v", err)
	}
	hub := daemonsource.NewHub(cli)
	defer hub.Stop()

	var redials int32
	hub.SetRedial(func() (*clientproto.Client, *protocol.Attached, error) {
		atomic.AddInt32(&redials, 1)
		return dial()
	})

	src, err := hub.NewTab(40, 10, "")
	if err != nil {
		t.Fatalf("new tab: %v", err)
	}
	tabID := src.TabID()

	src.Write([]byte("echo PREDROP\r"))
	if !waitViewport(src, "PREDROP", 8*time.Second) {
		t.Fatalf("pre-drop marker never rendered")
	}

	// Yank the connection out from under the hub.
	_ = cli.Close()

	// The hub must re-dial and bring the source back to life.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&redials) >= 1 && !src.IsReconnecting() {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if atomic.LoadInt32(&redials) < 1 {
		t.Fatalf("hub never re-dialed after connection drop")
	}
	if src.IsReconnecting() {
		t.Fatalf("source still marked reconnecting after successful redial")
	}

	// Identity must be stable: resync re-adopts the same Source, doesn't
	// replace it (so the GUI tab keeps working).
	if got := hub.Adopt(tabID, 40, 10); got != src {
		t.Fatalf("source identity changed across reconnect")
	}

	// And it must be writable + observable over the new connection.
	src.Write([]byte("echo POSTDROP\r"))
	if !waitViewport(src, "POSTDROP", 8*time.Second) {
		t.Fatalf("post-reconnect marker never rendered — resync did not rebind I/O")
	}
}

// waitViewport polls the Source's shadow viewport for needle.
func waitViewport(s *daemonsource.Source, needle string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if scanViewport(s, needle) {
			return true
		}
		select {
		case <-s.DataChan():
		case <-time.After(60 * time.Millisecond):
		}
	}
	return scanViewport(s, needle)
}

func scanViewport(s *daemonsource.Source, needle string) bool {
	grid := s.SnapshotViewport()
	for _, row := range grid {
		line := ""
		for _, c := range row {
			if c.Content == "" {
				continue
			}
			line += c.Content
		}
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}
