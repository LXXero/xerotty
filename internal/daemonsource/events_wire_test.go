package daemonsource

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/tabevents"
	"github.com/LXXero/xerotty/internal/testutil"
)

// TestHubWaitEvents reads a daemon's tab event ring over the wire,
// the path the GUI MCP aggregator uses for every host: a non-blocking
// read, a filtered read, a wait that wakes on a new event, and a
// cancelled wait.
func TestHubWaitEvents(t *testing.T) {
	sockPath := filepath.Join(testutil.SockDir(t), "xerottyd.sock")
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
	if _, err := cli.Hello("events-wire"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	go cli.Run()
	if err := cli.Attach("", false); err != nil {
		t.Fatalf("attach: %v", err)
	}
	<-cli.Attached()
	hub := NewHub(cli)
	defer hub.Stop()

	ring := d.Events()
	ring.Append(tabevents.Event{Kind: tabevents.Bell, Tab: 3, Name: "build"})
	code := 2
	ring.Append(tabevents.Event{Kind: tabevents.ChildExited, Tab: 4, ExitCode: &code})

	ctx := context.Background()
	res, err := hub.WaitEvents(ctx, tabevents.Query{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.Epoch != ring.Epoch() || res.Last < 2 || len(res.Events) < 2 {
		t.Fatalf("read = %+v", res)
	}

	res, err = hub.WaitEvents(ctx, tabevents.Query{Epoch: res.Epoch,
		Filter: tabevents.Filter{Kinds: []tabevents.Kind{tabevents.ChildExited}}})
	if err != nil {
		t.Fatalf("filtered read: %v", err)
	}
	if len(res.Events) != 1 || res.Events[0].Tab != 4 || res.Events[0].ExitCode == nil || *res.Events[0].ExitCode != 2 {
		t.Fatalf("filtered read = %+v", res)
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		ring.Append(tabevents.Event{Kind: tabevents.Notification, Tab: 3, Protocol: "osc9", NotifyBody: "done"})
	}()
	res, err = hub.WaitEvents(ctx, tabevents.Query{Epoch: res.Epoch, After: res.Last, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if len(res.Events) != 1 || res.Events[0].NotifyBody != "done" || res.Events[0].Protocol != "osc9" {
		t.Fatalf("wait = %+v", res)
	}

	cctx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	if _, err := hub.WaitEvents(cctx, tabevents.Query{Epoch: res.Epoch, After: res.Last, Timeout: 5 * time.Second}); err == nil {
		t.Fatalf("cancelled wait returned no error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("cancelled wait did not return promptly")
	}
}
