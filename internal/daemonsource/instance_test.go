package daemonsource_test

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/daemonsource"
	"github.com/LXXero/xerotty/internal/protocol"
)

// TestTombstoneDroppedOnDaemonRestart is the layer-4c finding-1
// regression: close-tombstones are scoped to the daemon INSTANCE. When
// the daemon restarts, its tab-id space resets to 1 — so a tab the user
// had closed (whose ID gets reused by a legitimate new tab on the fresh
// daemon) must NOT be suppressed forever. Reconnecting to a different
// Attached.InstanceID drops the stale tombstones.
//
// Without the fix the reused ID stays tombstoned: it's filtered out of
// every snapshot and SendTabClose is replayed forever, killing the new
// tab.
func TestTombstoneDroppedOnDaemonRestart(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")

	startDaemon := func() (*daemon.Daemon, chan error) {
		cfg := config.Default()
		d := daemon.New(&cfg, sockPath)
		done := make(chan error, 1)
		go func() { done <- d.Run() }()
		time.Sleep(50 * time.Millisecond)
		return d, done
	}

	dial := func() (*clientproto.Client, *protocol.Attached, error) {
		c, err := clientproto.Dial(sockPath)
		if err != nil {
			return nil, nil, err
		}
		if _, err := c.Hello("inst"); err != nil {
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

	d1, done1 := startDaemon()

	cli, att1, err := dial()
	if err != nil {
		t.Fatalf("initial dial: %v", err)
	}
	hub := daemonsource.NewHub(cli)
	defer hub.Stop()
	hub.SeedInstance(att1.InstanceID) // baseline = d1's identity

	// Gate the first reconnect so we control the restart sequencing.
	firstStarted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	hub.SetRedial(func() (*clientproto.Client, *protocol.Attached, error) {
		once.Do(func() {
			close(firstStarted)
			<-release
		})
		return dial()
	})

	src, err := hub.NewTab(40, 10, "")
	if err != nil {
		t.Fatalf("tab on d1: %v", err)
	}
	tabID := src.TabID()

	// Drop the connection and stop d1, then close the tab while down —
	// the close can't reach d1, so it's tombstoned.
	_ = cli.Close()
	<-firstStarted
	src.Close()
	_ = d1.Stop()
	<-done1

	// Bring up a FRESH daemon on the same socket (new InstanceID, tab-id
	// space reset to 1) and create a brand-new tab that REUSES the id.
	d2, done2 := startDaemon()
	defer func() { _ = d2.Stop(); <-done2 }()

	probe, att2, err := dial()
	if err != nil {
		t.Fatalf("probe dial to d2: %v", err)
	}
	defer probe.Close()
	if att2.InstanceID == att1.InstanceID {
		t.Fatalf("restarted daemon reused InstanceID %q — identity nonce not per-process", att2.InstanceID)
	}
	pHub := daemonsource.NewHub(probe)
	defer pHub.Stop()
	newSrc, err := pHub.NewTab(40, 10, "")
	if err != nil {
		t.Fatalf("tab on d2: %v", err)
	}
	if newSrc.TabID() != tabID {
		t.Fatalf("expected d2 to reuse tab id %d, got %d (test assumption broken)", tabID, newSrc.TabID())
	}

	// Now let the gated reconnect proceed → hub reconnects to d2.
	close(release)

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && src.IsReconnecting() {
		time.Sleep(40 * time.Millisecond)
	}

	// The reused tab must be adopted on the hub, NOT suppressed.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sourceIDs(hub)[tabID] {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !sourceIDs(hub)[tabID] {
		t.Fatalf("reused tab %d was suppressed after daemon restart — tombstone not scoped to instance", tabID)
	}

	// And the new tab must still be alive on d2 (no replayed close killed
	// it): a fresh probe should still see it after a moment.
	time.Sleep(300 * time.Millisecond)
	verify, attV, err := dial()
	if err != nil {
		t.Fatalf("verify dial: %v", err)
	}
	defer verify.Close()
	found := false
	for _, ti := range attV.Tabs {
		if ti.ID == tabID {
			found = true
		}
	}
	if !found {
		t.Fatalf("tab %d was killed on d2 by a replayed tombstone close", tabID)
	}
}
