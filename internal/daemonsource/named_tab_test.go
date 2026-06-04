package daemonsource_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/daemonsource"
)

// TestNamedTabCreateReuses covers the wire-level idempotent create
// (protocol v8 TabCreate.Name): two NewNamedTab calls under the same
// label must yield the SAME daemon tab, the second flagged reused —
// the semantics the GUI's aggregating MCP create_tab leans on.
func TestNamedTabCreateReuses(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "xerottyd.sock")
	cfg := config.Default()
	d := daemon.New(&cfg, sockPath)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	defer func() { _ = d.Stop(); <-doneRun }()
	time.Sleep(50 * time.Millisecond)

	cli, err := clientproto.Dial(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()
	cli.Hello("named-tab-test")
	go cli.Run()
	cli.Attach("", false)
	<-cli.Attached()

	hub := daemonsource.NewHub(cli)
	defer hub.Stop()

	src1, reused1, err := hub.NewNamedTab("build", 80, 24, "")
	if err != nil {
		t.Fatalf("first named create: %v", err)
	}
	if reused1 {
		t.Fatalf("first create must not be reused")
	}
	src2, reused2, err := hub.NewNamedTab("build", 80, 24, "")
	if err != nil {
		t.Fatalf("second named create: %v", err)
	}
	if !reused2 {
		t.Fatalf("second create under same name must report reused")
	}
	if src1.TabID() != src2.TabID() {
		t.Fatalf("reuse returned a different tab: %d vs %d", src1.TabID(), src2.TabID())
	}

	// Different name → fresh tab.
	src3, reused3, err := hub.NewNamedTab("logs", 80, 24, "")
	if err != nil {
		t.Fatalf("third named create: %v", err)
	}
	if reused3 || src3.TabID() == src1.TabID() {
		t.Fatalf("different name must mint a fresh tab (reused=%v id=%d)", reused3, src3.TabID())
	}
}
