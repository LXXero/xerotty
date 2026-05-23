package daemonsource_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/protocol"
)

// TestProposalGateWire exercises the propose-mode GUI gate's wire
// path: queue a proposal on the daemon (simulating an MCP agent),
// confirm a wire client receives ProposalsChanged, then resolve
// it (approve) and confirm the queue empties.
func TestProposalGateWire(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	cli, _ := clientproto.Dial(sockPath)
	defer cli.Close()
	cli.Hello("gate-test")
	go cli.Run()
	cli.Attach("", true)
	attached := <-cli.Attached()
	tabID := attached.Tabs[0].ID

	// Drain cell traffic so the daemon's send loop doesn't block.
	go func() {
		for {
			select {
			case <-cli.CellFull():
			case <-cli.CellDiff():
			case <-cli.Cursor():
			case <-cli.Closed():
				return
			}
		}
	}()

	// Drain the attach-time ProposalsChanged (empty).
	drainProposals(cli, 1*time.Second)

	// Simulate an MCP agent queuing a propose-mode write.
	sess := d.SessionByName("default")
	sess.QueueProposedInput(tabID, []byte("rm -rf /tmp/whatever\r"))

	// Client should receive a ProposalsChanged with 1 entry.
	pc := waitProposals(t, cli, 3*time.Second)
	if len(pc.Proposals) != 1 {
		t.Fatalf("expected 1 proposal, got %d", len(pc.Proposals))
	}
	p := pc.Proposals[0]
	if p.TabID != tabID || p.Kind != "input" {
		t.Errorf("proposal mismatch: %+v", p)
	}
	if p.Preview == "" {
		t.Errorf("expected a non-empty preview")
	}

	// Resolve (drop) it from the "GUI".
	if err := cli.SendProposalResolve(p.Index, false); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Queue should empty → ProposalsChanged with 0 entries.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pc := waitProposals(t, cli, 1*time.Second)
		if pc != nil && len(pc.Proposals) == 0 {
			return
		}
	}
	t.Fatal("queue never emptied after drop")
}

func drainProposals(cli *clientproto.Client, d time.Duration) {
	to := time.After(d)
	for {
		select {
		case <-cli.ProposalsChanged():
		case <-to:
			return
		}
	}
}

func waitProposals(t *testing.T, cli *clientproto.Client, d time.Duration) *protocol.ProposalsChanged {
	t.Helper()
	select {
	case pc := <-cli.ProposalsChanged():
		return pc
	case <-time.After(d):
		return nil
	}
}
