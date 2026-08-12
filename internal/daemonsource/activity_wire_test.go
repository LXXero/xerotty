package daemonsource

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
)

// TestSourceActivityClock confirms the daemon's per-tab activity clock
// reaches the client Source over the wire: after input + output, both
// LastInput and LastOutput read recent (client clock, anchored from
// the shipped ages + bumped by cell diffs).
func TestSourceActivityClock(t *testing.T) {
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
	if _, err := cli.Hello("activity-test"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	go cli.Run()
	if err := cli.Attach("", false); err != nil {
		t.Fatalf("attach: %v", err)
	}
	<-cli.Attached()

	hub := NewHub(cli)
	defer hub.Stop()
	src, err := hub.NewTab(80, 24, "")
	if err != nil {
		t.Fatalf("new tab: %v", err)
	}

	// Let the shell's prompt output flow (drives LastOutput via cell
	// diffs + TabState), then type (drives LastInput on the daemon).
	time.Sleep(300 * time.Millisecond)
	if _, err := src.Write([]byte("echo activity\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Poll until both stamps show up and are recent.
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		lo, li := src.LastOutput(), src.LastInput()
		if !lo.IsZero() && !li.IsZero() &&
			time.Since(lo) < 5*time.Second && time.Since(li) < 5*time.Second {
			return // success
		}
		select {
		case <-src.DataChan():
		case <-time.After(150 * time.Millisecond):
		}
	}
	lo, li := src.LastOutput(), src.LastInput()
	t.Fatalf("activity clock not live: lastOutput=%v (age %v) lastInput=%v (age %v)",
		lo, time.Since(lo), li, time.Since(li))
}
