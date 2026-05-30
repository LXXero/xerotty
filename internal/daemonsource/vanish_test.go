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

// TestVanishDistinguishesRestartFromRemoteClose is the finding-1
// regression: the GUI reseat (keep the window alive on a daemon restart
// instead of quitting) must fire ONLY on a genuine daemon RESTART, never
// on a remote/MCP close of the last tab on a still-LIVE daemon. The seam
// the GUI gates on is Source.VanishedByRestart(): both vanish paths set
// IsVanished() (the tab is gone either way), but only a reattach that
// observed a new daemon InstanceID sets VanishedByRestart().
//
// Without the fix both paths looked identical (generic IsVanished), so a
// remote close that emptied a window reseated a phantom tab instead of
// letting the window close.
func TestVanishRestartVsRemoteClose(t *testing.T) {
	t.Run("remote", func(t *testing.T) {
		sockPath := filepath.Join(t.TempDir(), "d.sock")
		cfg := config.Default()
		d := daemon.New(&cfg, sockPath)
		done := make(chan error, 1)
		go func() { done <- d.Run() }()
		defer func() { _ = d.Stop(); <-done }()
		time.Sleep(50 * time.Millisecond)

		dial := func(name string) (*clientproto.Client, *protocol.Attached) {
			c, att, err := dialAttach(sockPath, name)
			if err != nil {
				t.Fatalf("dial %s: %v", name, err)
			}
			return c, att
		}

		cli, att := dial("owner")
		hub := daemonsource.NewHub(cli)
		defer hub.Stop()
		hub.SeedInstance(att.InstanceID)

		src, err := hub.NewTab(40, 10, "")
		if err != nil {
			t.Fatalf("new tab: %v", err)
		}
		tabID := src.TabID()

		// A DIFFERENT client closes the tab on the still-live daemon —
		// the owner hub learns about it via a topology broadcast.
		other, _ := dial("other")
		defer other.Close()
		if err := other.SendTabClose(tabID); err != nil {
			t.Fatalf("remote close: %v", err)
		}

		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && !src.IsVanished() {
			time.Sleep(40 * time.Millisecond)
		}
		if !src.IsVanished() {
			t.Fatalf("tab %d never vanished after remote close", tabID)
		}
		if src.VanishedByRestart() {
			t.Fatalf("remote close on a LIVE daemon was misreported as a restart — GUI would reseat a phantom tab instead of closing the window")
		}
	})

	t.Run("restart", func(t *testing.T) {
		sockPath := filepath.Join(t.TempDir(), "d.sock")

		startDaemon := func() (*daemon.Daemon, chan error) {
			cfg := config.Default()
			d := daemon.New(&cfg, sockPath)
			done := make(chan error, 1)
			go func() { done <- d.Run() }()
			time.Sleep(50 * time.Millisecond)
			return d, done
		}

		d1, done1 := startDaemon()
		cli, att1, err := dialAttach(sockPath, "owner")
		if err != nil {
			t.Fatalf("initial dial: %v", err)
		}
		hub := daemonsource.NewHub(cli)
		defer hub.Stop()
		hub.SeedInstance(att1.InstanceID)

		// Gate the reconnect so we restart the daemon before the redial.
		firstStarted := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		hub.SetRedial(func() (*clientproto.Client, *protocol.Attached, error) {
			once.Do(func() {
				close(firstStarted)
				<-release
			})
			return dialAttach(sockPath, "owner")
		})

		src, err := hub.NewTab(40, 10, "")
		if err != nil {
			t.Fatalf("tab on d1: %v", err)
		}

		// Drop the link, stop d1, bring up a FRESH daemon (new InstanceID).
		_ = cli.Close()
		<-firstStarted
		_ = d1.Stop()
		<-done1

		d2, done2 := startDaemon()
		defer func() { _ = d2.Stop(); <-done2 }()

		// Let the gated reconnect proceed → resync sees a new InstanceID,
		// the old tab is absent → restart-vanish.
		close(release)

		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) && !src.IsVanished() {
			time.Sleep(40 * time.Millisecond)
		}
		if !src.IsVanished() {
			t.Fatalf("tab never vanished after daemon restart")
		}
		if !src.VanishedByRestart() {
			t.Fatalf("daemon restart was NOT flagged as a restart — GUI would quit instead of reseating")
		}
	})
}

// dialAttach connects, says hello, runs the client, attaches, and waits
// for the Attached frame. Shared by the vanish-path subtests.
func dialAttach(sockPath, name string) (*clientproto.Client, *protocol.Attached, error) {
	c, err := clientproto.Dial(sockPath)
	if err != nil {
		return nil, nil, err
	}
	if _, err := c.Hello(name); err != nil {
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
