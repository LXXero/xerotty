package app

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemon"
	"github.com/LXXero/xerotty/internal/protocol"
	"github.com/LXXero/xerotty/internal/testutil"
)

// The remote dial must never block the render thread: withRemoteHub
// returns at once with the dialog in its connecting state, and the
// result is applied by pollRemoteDials on a later frame.

func startLocalDaemon(t *testing.T) string {
	t.Helper()
	sockPath := filepath.Join(testutil.SockDir(t), "xerottyd.sock")
	cfg := config.Default()
	cfg.Shell = "/bin/sh"
	d := daemon.New(&cfg, sockPath)
	doneRun := make(chan error, 1)
	go func() { doneRun <- d.Run() }()
	t.Cleanup(func() { _ = d.Stop(); <-doneRun })
	time.Sleep(50 * time.Millisecond)
	return sockPath
}

// localDialer reaches the test daemon over its unix socket and runs
// the real handshake half of dialRemoteDaemon, cancel watcher
// included; only ssh is left out.
func localDialer(a *App, sockPath string) remoteDialFunc {
	return func(name string, _ config.RemoteHost, cancel <-chan struct{}) (*clientproto.Client, *protocol.Attached, error) {
		cli, err := clientproto.Dial(sockPath)
		if err != nil {
			return nil, nil, err
		}
		return a.finishRemoteDial(name, cli, cancel)
	}
}

// pollUntil runs pollRemoteDials, as the frame loop would, until
// cond holds or the deadline passes.
func pollUntil(t *testing.T, a *App, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a.pollRemoteDials()
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

func newDialTestApp(t *testing.T) (*App, *Window) {
	t.Helper()
	a := &App{cfg: config.Default()}
	w := &Window{app: a}
	a.windows = append(a.windows, w)
	t.Cleanup(func() {
		a.remoteHubsMu.Lock()
		defer a.remoteHubsMu.Unlock()
		for _, e := range a.remoteHubs {
			e.hub.Stop()
			_ = e.hub.Client().Close()
		}
	})
	return a, w
}

func TestRemoteDialConnectsOffTheRenderThread(t *testing.T) {
	sockPath := startLocalDaemon(t)
	a, w := newDialTestApp(t)
	a.remoteDialer = localDialer(a, sockPath)

	var got *remoteHubEntry
	w.withRemoteHub("kh", func(e *remoteHubEntry) error {
		got = e
		return nil
	})
	// Returned without the hub: the dial is in flight, the dialog
	// says so.
	if got != nil {
		t.Fatal("use ran before the dial completed")
	}
	if !w.connectingHost || !w.connectPending || w.connectTarget != "kh" {
		t.Fatalf("dialog not in connecting state: open=%v pending=%v target=%q", w.connectingHost, w.connectPending, w.connectTarget)
	}
	pollUntil(t, a, func() bool { return got != nil })
	if a.lookupRemoteHub("kh") != got {
		t.Fatal("hub not registered under the host name")
	}
	if w.connectingHost || w.connectPending || w.connectError != "" {
		t.Fatalf("dialog not closed after connect: open=%v pending=%v err=%q", w.connectingHost, w.connectPending, w.connectError)
	}
	// The handshake really finished: the hub can create a tab.
	src, err := got.hub.NewTabIn(0, 80, 24, "", nil)
	if err != nil {
		t.Fatalf("NewTabIn on the dialed hub: %v", err)
	}
	src.Close()

	// A second request for the same host is served at once.
	var again *remoteHubEntry
	w.withRemoteHub("kh", func(e *remoteHubEntry) error {
		again = e
		return nil
	})
	if again != got {
		t.Fatal("second request did not reuse the registered hub synchronously")
	}
}

func TestRemoteDialCancel(t *testing.T) {
	a, w := newDialTestApp(t)
	released := make(chan struct{})
	a.remoteDialer = func(_ string, _ config.RemoteHost, cancel <-chan struct{}) (*clientproto.Client, *protocol.Attached, error) {
		<-cancel
		close(released)
		return nil, nil, errors.New("ssh: killed")
	}
	used := false
	w.withRemoteHub("kh", func(*remoteHubEntry) error {
		used = true
		return nil
	})
	a.pollRemoteDials()
	if !w.connectPending {
		t.Fatal("dial should still be pending")
	}
	a.cancelRemoteDial("kh")
	if used {
		t.Fatal("use ran for a cancelled dial")
	}
	if w.connectingHost || w.connectPending {
		t.Fatalf("cancel should close the dialog: open=%v pending=%v", w.connectingHost, w.connectPending)
	}
	if _, ok := a.remoteDials["kh"]; ok {
		t.Fatal("cancelled dial still registered as in flight")
	}
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not reach the dialer")
	}
	pollUntil(t, a, func() bool { return len(a.remoteDialsDraining) == 0 })
	if a.lookupRemoteHub("kh") != nil {
		t.Fatal("cancelled dial registered a hub")
	}
}

// A cancel that lands just as ssh finishes: the connection the dial
// hands back is closed, not leaked.
func TestRemoteDialCancelClosesLateConnection(t *testing.T) {
	sockPath := startLocalDaemon(t)
	a, w := newDialTestApp(t)
	late := make(chan *clientproto.Client, 1)
	a.remoteDialer = func(name string, _ config.RemoteHost, cancel <-chan struct{}) (*clientproto.Client, *protocol.Attached, error) {
		<-cancel
		cli, err := clientproto.Dial(sockPath)
		if err != nil {
			return nil, nil, err
		}
		cli, att, err := a.finishRemoteDial(name, cli, nil)
		if err == nil {
			late <- cli
		}
		return cli, att, err
	}
	w.withRemoteHub("kh", func(*remoteHubEntry) error { return nil })
	a.cancelRemoteDial("kh")
	var cli *clientproto.Client
	select {
	case cli = <-late:
	case <-time.After(5 * time.Second):
		t.Fatal("late connection never completed")
	}
	pollUntil(t, a, func() bool { return len(a.remoteDialsDraining) == 0 })
	select {
	case <-cli.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("late connection was not closed")
	}
	if a.lookupRemoteHub("kh") != nil {
		t.Fatal("cancelled dial registered a hub")
	}
}

func TestRemoteDialFailureShowsError(t *testing.T) {
	a, w := newDialTestApp(t)
	a.remoteDialer = func(string, config.RemoteHost, <-chan struct{}) (*clientproto.Client, *protocol.Attached, error) {
		return nil, nil, errors.New("ssh: Could not resolve hostname kh")
	}
	used := false
	w.withRemoteHub("kh", func(*remoteHubEntry) error {
		used = true
		return nil
	})
	pollUntil(t, a, func() bool { return !w.connectPending })
	if used {
		t.Fatal("use ran after a failed dial")
	}
	if !w.connectingHost || !strings.Contains(w.connectError, "Could not resolve hostname") {
		t.Fatalf("dialog should stay open with the error: open=%v err=%q", w.connectingHost, w.connectError)
	}
	if _, ok := a.remoteDials["kh"]; ok {
		t.Fatal("failed dial still registered as in flight")
	}
}

// The window that asked for the host may be gone by the time ssh
// answers. The hub is still registered (another window can use it);
// the request itself is dropped.
func TestRemoteDialSkipsClosedWindow(t *testing.T) {
	sockPath := startLocalDaemon(t)
	a, w := newDialTestApp(t)
	a.remoteDialer = localDialer(a, sockPath)
	used := false
	w.withRemoteHub("kh", func(*remoteHubEntry) error {
		used = true
		return nil
	})
	w.pendingClose = true
	pollUntil(t, a, func() bool { return a.lookupRemoteHub("kh") != nil })
	if used {
		t.Fatal("use ran for a closed window")
	}
}
