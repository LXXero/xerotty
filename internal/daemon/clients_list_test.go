package daemon_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/protocol"
)

// TestClientsListAndKick exercises the wire path behind the GUI's
// Clients menu: list every attached client (with the requester's own
// entry marked You) and force-disconnect one by id.
func TestClientsListAndKick(t *testing.T) {
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

	dial := func(id string) *clientproto.Client {
		t.Helper()
		c, err := clientproto.Dial(sockPath)
		if err != nil {
			t.Fatalf("dial %s: %v", id, err)
		}
		if _, err := c.Hello(id); err != nil {
			t.Fatalf("hello %s: %v", id, err)
		}
		go c.Run()
		return c
	}

	alpha := dial("alpha")
	defer alpha.Close()
	beta := dial("beta")
	defer beta.Close()

	list := func(reqID uint64) []protocol.ClientInfo {
		t.Helper()
		if err := alpha.SendClientsListReq(reqID); err != nil {
			t.Fatalf("SendClientsListReq: %v", err)
		}
		select {
		case cl := <-alpha.ClientsLists():
			if cl.ReqID != reqID {
				t.Fatalf("reply ReqID = %d, want %d", cl.ReqID, reqID)
			}
			return cl.Clients
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for ClientsList")
			return nil
		}
	}

	infos := list(1)
	if len(infos) != 2 {
		t.Fatalf("clients = %d, want 2 (%v)", len(infos), infos)
	}
	var sawYou, sawBeta bool
	for _, ci := range infos {
		switch ci.ClientID {
		case "alpha":
			if !ci.You {
				t.Error("alpha's own entry not marked You")
			}
			sawYou = true
		case "beta":
			if ci.You {
				t.Error("beta marked You on alpha's request")
			}
			sawBeta = true
		}
	}
	if !sawYou || !sawBeta {
		t.Fatalf("missing expected entries: %v", infos)
	}

	// Kick beta; its connection must close.
	if err := alpha.SendClientKick("beta"); err != nil {
		t.Fatalf("SendClientKick: %v", err)
	}
	select {
	case <-beta.Closed():
	case <-time.After(2 * time.Second):
		t.Fatal("beta was not disconnected within 2s of the kick")
	}

	// The list converges to alpha alone. Poll briefly — unregister
	// runs on beta's conn teardown, which races the Closed signal.
	deadline := time.Now().Add(2 * time.Second)
	for reqID := uint64(2); ; reqID++ {
		infos = list(reqID)
		if len(infos) == 1 && infos[0].ClientID == "alpha" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("post-kick clients = %v, want just alpha", infos)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
