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

// TestCloseTombstoneSurvivesReconnect is the layer-4c regression for the
// documented pitfall: a tab the user closes WHILE the daemon connection
// is down must not be resurrected by the snapshot resync on reconnect.
//
// The close's MsgTabClose is lost (the connection is dead), so the
// daemon still has the tab when we reconnect; the Hub must replay the
// close from its tombstone and keep the tab filtered out — locally and,
// after the replay lands, on the daemon too.
func TestCloseTombstoneSurvivesReconnect(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	dial := func(id string) (*clientproto.Client, *protocol.Attached, error) {
		c, err := clientproto.Dial(sockPath)
		if err != nil {
			return nil, nil, err
		}
		if _, err := c.Hello(id); err != nil {
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

	cli, _, err := dial("tomb")
	if err != nil {
		t.Fatalf("initial dial: %v", err)
	}
	hub := daemonsource.NewHub(cli)
	defer hub.Stop()

	// Gate the FIRST reconnect so the test can close a tab during the
	// disconnected window; later reconnects (if any) run free.
	firstStarted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	hub.SetRedial(func() (*clientproto.Client, *protocol.Attached, error) {
		once.Do(func() {
			close(firstStarted)
			<-release
		})
		return dial("tomb")
	})

	src1, err := hub.NewTab(40, 10, "")
	if err != nil {
		t.Fatalf("tab 1: %v", err)
	}
	src2, err := hub.NewTab(40, 10, "")
	if err != nil {
		t.Fatalf("tab 2: %v", err)
	}
	tab1, tab2 := src1.TabID(), src2.TabID()

	// Drop the connection, then close tab 1 while the link is down — the
	// MsgTabClose can't reach the daemon.
	_ = cli.Close()
	<-firstStarted
	src1.Close()
	close(release)

	// Wait for the reconnect to complete.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && src2.IsReconnecting() {
		time.Sleep(40 * time.Millisecond)
	}
	if src2.IsReconnecting() {
		t.Fatalf("reconnect never completed")
	}

	// Local: tab 1 must NOT be resurrected; tab 2 must still be present.
	if got := sourceIDs(hub); got[tab1] {
		t.Fatalf("tombstoned tab %d was resurrected on the hub after reconnect", tab1)
	}
	if got := sourceIDs(hub); !got[tab2] {
		t.Fatalf("surviving tab %d went missing after reconnect", tab2)
	}

	// Daemon-side: the replayed close must actually remove tab 1. A fresh
	// probe client should eventually see only tab 2.
	probeDeadline := time.Now().Add(8 * time.Second)
	for {
		pc, att, err := dial("probe")
		if err != nil {
			t.Fatalf("probe dial: %v", err)
		}
		has1, has2 := false, false
		for _, ti := range att.Tabs {
			if ti.ID == tab1 {
				has1 = true
			}
			if ti.ID == tab2 {
				has2 = true
			}
		}
		_ = pc.Close()
		if !has1 && has2 {
			return // tombstone replay killed tab 1 daemon-side; tab 2 lives
		}
		if time.Now().After(probeDeadline) {
			t.Fatalf("daemon still has closed tab %d (has1=%v has2=%v) — replay did not land", tab1, has1, has2)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func sourceIDs(h *daemonsource.Hub) map[uint32]bool {
	out := map[uint32]bool{}
	for _, s := range h.Sources() {
		out[s.TabID()] = true
	}
	return out
}
