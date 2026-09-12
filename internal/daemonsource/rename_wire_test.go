package daemonsource

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
)

// TestTabRenameSyncsAcrossClients guards the daemon-authoritative
// label flow (protocol v10): a rename issued by one client must reach
// the daemon's Tab (so both MCP sockets agree) and propagate to every
// OTHER attached client via TabState — the divergence that used to
// let a serve --upgrade reattach wipe GUI-only labels.
func TestTabRenameSyncsAcrossClients(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	dial := func(id string) *clientproto.Client {
		cli, err := clientproto.Dial(sockPath)
		if err != nil {
			t.Fatalf("%s dial: %v", id, err)
		}
		t.Cleanup(func() { cli.Close() })
		if _, err := cli.Hello(id); err != nil {
			t.Fatalf("%s hello: %v", id, err)
		}
		go cli.Run()
		if err := cli.Attach("", false); err != nil {
			t.Fatalf("%s attach: %v", id, err)
		}
		<-cli.Attached()
		return cli
	}

	cliA := dial("rename-a")
	hubA := NewHub(cliA)
	defer hubA.Stop()
	srcA, err := hubA.NewTab(80, 24, "")
	if err != nil {
		t.Fatalf("new tab: %v", err)
	}

	cliB := dial("rename-b")
	hubB := NewHub(cliB)
	defer hubB.Stop()
	srcB := hubB.Adopt(srcA.TabID(), 80, 24)
	if srcB == nil {
		t.Fatalf("adopt tab %d on client B", srcA.TabID())
	}

	srcA.Rename("build-tab")
	if got := srcA.Name(); got != "build-tab" {
		t.Fatalf("optimistic local name = %q, want %q", got, "build-tab")
	}

	// The daemon broadcasts the new name via TabState (wakeTabSubs
	// makes it prompt); client B's Source mirror must converge.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if srcB.Name() == "build-tab" {
			return // success
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("client B never saw the rename: Name() = %q", srcB.Name())
}
