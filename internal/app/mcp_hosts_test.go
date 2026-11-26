package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/daemonsource"
	"github.com/LXXero/xerotty/internal/protocol"
	"github.com/LXXero/xerotty/internal/tabs"
)

// runFrames plays the frame loop's part for an MCP request: run the
// calls handed to the render thread and apply finished dials, until
// done is closed.
func runFrames(t *testing.T, a *App, done <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return
		default:
		}
		a.drainMainCalls()
		a.pollRemoteDials()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("MCP request not answered before deadline")
}

// attachAsync runs AttachHost on its own goroutine, the way the GUI
// MCP server calls it.
func attachAsync(a *App, host string) (done chan struct{}, already *bool, err *error) {
	done = make(chan struct{})
	already, err = new(bool), new(error)
	go func() {
		*already, *err = a.AttachHost(context.Background(), host)
		close(done)
	}()
	return done, already, err
}

func TestAttachHostDialsThroughTheRemoteMenuPath(t *testing.T) {
	sockPath := startLocalDaemon(t)
	a, w := newDialTestApp(t)
	w.tabs = tabs.NewManager(&a.cfg)
	other := &Window{app: a, tabs: tabs.NewManager(&a.cfg)}
	a.windows = append(a.windows, other)
	a.active = w

	// A tab the host's daemon already has, to be adopted.
	pre, _, err := localDialer(a, sockPath)("pre", config.RemoteHost{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	preHub := daemonsource.NewHub(pre)
	preTab, err := preHub.NewTabIn(0, 80, 24, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	preHub.Stop()
	_ = pre.Close()

	// The dial waits on gate, so both calls below join one dial and
	// the window that showed it can close first.
	var dials atomic.Int32
	gate := make(chan struct{})
	dialer := localDialer(a, sockPath)
	a.remoteDialer = func(name string, h config.RemoteHost, cancel <-chan struct{}) (*clientproto.Client, *protocol.Attached, error) {
		dials.Add(1)
		<-gate
		return dialer(name, h, cancel)
	}

	done1, already1, err1 := attachAsync(a, "xhtpc")
	done2, already2, err2 := attachAsync(a, "xhtpc")
	joined := func() bool { d := a.remoteDials["xhtpc"]; return d != nil && len(d.waiters) == 2 }
	deadline := time.Now().Add(5 * time.Second)
	for !joined() && time.Now().Before(deadline) {
		a.drainMainCalls()
		time.Sleep(5 * time.Millisecond)
	}
	if !joined() {
		t.Fatal("the two attaches are not waiting on one dial")
	}
	// An agent's attach shows no connect dialog: it would hold the
	// keyboard of the user's window.
	if w.connectingHost || w.connectPending {
		t.Fatalf("attach opened the connect dialog: open=%v pending=%v", w.connectingHost, w.connectPending)
	}
	w.pendingClose = true
	close(gate)
	runFrames(t, a, done1)
	runFrames(t, a, done2)
	if *err1 != nil || *err2 != nil || *already1 || *already2 {
		t.Fatalf("concurrent attaches: (%v, %v) (%v, %v); want two fresh attaches", *already1, *err1, *already2, *err2)
	}
	if n := dials.Load(); n != 1 {
		t.Fatalf("concurrent attaches dialed %d times, want 1", n)
	}
	if a.lookupRemoteHub("xhtpc") == nil || !slices.Contains(a.EventHosts(), "xhtpc") {
		t.Fatalf("hub not registered under the host name: %v", a.EventHosts())
	}
	// The dialing window closed, so the host's tab went to the other
	// one, labelled with its host as a manual Reattach labels it.
	if len(w.tabs.Tabs) != 0 || len(other.tabs.Tabs) != 1 {
		t.Fatalf("adopted tabs: closed window %d, open window %d; want 0 and 1", len(w.tabs.Tabs), len(other.tabs.Tabs))
	}
	got := other.tabs.Tabs[0]
	if ds, ok := got.Terminal.(*daemonsource.Source); !ok || ds.TabID() != preTab.TabID() || got.Host != "xhtpc" {
		t.Fatalf("adopted tab = %+v (host %q), want daemon tab %d on xhtpc", got.Terminal, got.Host, preTab.TabID())
	}

	// Attached now: answered without the render thread or a dial.
	already, err := a.AttachHost(context.Background(), "xhtpc")
	if err != nil || !already || dials.Load() != 1 {
		t.Fatalf("repeat AttachHost = already %v, err %v, dials %d", already, err, dials.Load())
	}
}

func TestAttachHostReportsDialFailure(t *testing.T) {
	a, w := newDialTestApp(t)
	a.active = w
	a.cfg.Hosts = []config.RemoteHost{{Name: "my box", SSHDest: "xhtpc"}}
	var dialed []string
	a.remoteDialer = func(name string, _ config.RemoteHost, _ <-chan struct{}) (*clientproto.Client, *protocol.Attached, error) {
		dialed = append(dialed, name)
		return nil, nil, errors.New("ssh: Could not resolve hostname nope")
	}

	// Values that could be ssh options or reach a shell never dial;
	// neither does "local" when there is no local hub.
	for _, bad := range []string{"-oProxyCommand=touch x", "-p", "a b", " xhtpc", "x=y", "h;id", "h$(id)", "h`id`",
		"h|id", "a@b@c", "user@-x", "@h", "h:0", "[::1]:22", "ssh://h", "h/x", "h%d", "local"} {
		done, _, err := attachAsync(a, bad)
		runFrames(t, a, done)
		if *err == nil {
			t.Errorf("AttachHost(%q) succeeded", bad)
		}
	}
	if len(dialed) != 0 {
		t.Fatalf("refused hosts reached the dialer: %q", dialed)
	}

	// A [[hosts]] name dials with its saved settings, space and all;
	// the dial's own error comes back to the caller, and no dialog is
	// left open in the user's window.
	done, _, err := attachAsync(a, "my box")
	runFrames(t, a, done)
	if *err == nil || !strings.Contains((*err).Error(), "Could not resolve hostname") || !slices.Equal(dialed, []string{"my box"}) {
		t.Fatalf("AttachHost(bookmark) err = %v, dialed %q", *err, dialed)
	}
	if w.connectingHost || w.connectPending || w.connectError != "" {
		t.Fatalf("failed attach left a dialog: open=%v pending=%v err=%q", w.connectingHost, w.connectPending, w.connectError)
	}

	// A dial that outlasts the timeout is cancelled, leaving no hub
	// and no open dialog.
	attachHostTimeout = 100 * time.Millisecond
	t.Cleanup(func() { attachHostTimeout = 60 * time.Second })
	cancelled := make(chan struct{})
	a.remoteDialer = func(_ string, _ config.RemoteHost, cancel <-chan struct{}) (*clientproto.Client, *protocol.Attached, error) {
		<-cancel
		close(cancelled)
		return nil, nil, errors.New("ssh: killed")
	}
	done, _, err = attachAsync(a, "slow")
	runFrames(t, a, done)
	if *err == nil || !strings.Contains((*err).Error(), "abandoned") {
		t.Fatalf("slow AttachHost err = %v", *err)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed-out attach did not cancel its dial")
	}
	pollUntil(t, a, func() bool { return len(a.remoteDialsDraining) == 0 })
	if a.lookupRemoteHub("slow") != nil || w.connectPending || w.connectingHost {
		t.Fatalf("after the timeout: hub %v, dialog pending=%v open=%v", a.lookupRemoteHub("slow") != nil, w.connectPending, w.connectingHost)
	}

	// The same timeout leaves alone a dial that a manual Reattach is
	// waiting on too: the user still wants it.
	cancelled = make(chan struct{})
	done, _, err = attachAsync(a, "shared")
	deadline := time.Now().Add(5 * time.Second)
	for a.remoteDials["shared"] == nil && time.Now().Before(deadline) {
		a.drainMainCalls()
		time.Sleep(5 * time.Millisecond)
	}
	manual := false
	w.withRemoteHub("shared", func(*remoteHubEntry) error { manual = true; return nil })
	runFrames(t, a, done)
	if *err == nil {
		t.Fatal("shared AttachHost should time out")
	}
	select {
	case <-cancelled:
		t.Fatal("an MCP timeout cancelled a dial a manual Reattach was waiting on")
	case <-time.After(200 * time.Millisecond):
	}
	if a.remoteDials["shared"] == nil {
		t.Fatal("shared dial no longer in flight")
	}
	a.cancelRemoteDial("shared")
	<-cancelled
	pollUntil(t, a, func() bool { return len(a.remoteDialsDraining) == 0 })
	if manual {
		t.Fatal("manual request ran for a cancelled dial")
	}

	// A render thread that never answers bounds the call, and the
	// error says the outcome is unknown rather than that it failed.
	attachAbandonWait = 100 * time.Millisecond
	t.Cleanup(func() { attachAbandonWait = 30 * time.Second })
	cancelled = make(chan struct{})
	_, stuckErr := a.AttachHost(context.Background(), "stuck")
	if stuckErr == nil || !strings.Contains(stuckErr.Error(), "may still complete") {
		t.Fatalf("AttachHost with no render thread: %v", stuckErr)
	}
	// Run the queued work, then end the dial it starts.
	a.drainMainCalls()
	a.cancelRemoteDial("stuck")
	<-cancelled
	pollUntil(t, a, func() bool { return len(a.remoteDialsDraining) == 0 })
}

func TestSSHConfigHosts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config")
	cfg := "Host *\n  ServerAliveInterval 60\nHost kh zx # two names\n  HostName kh.example\n" +
		"host=xwork\nHost !bad 192.168.1.1 web?\nMatch host foo\nHost kh\n" +
		"Host \"quoted\" 'single'\n\tHost = spaced\n# Host commented\n"
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	got := sshConfigHosts(p)
	want := []string{"kh", "zx", "xwork", "192.168.1.1", "quoted", "single", "spaced"}
	if !slices.Equal(got, want) {
		t.Fatalf("sshConfigHosts = %v, want %v", got, want)
	}
	if sshConfigHosts(filepath.Join(t.TempDir(), "missing")) != nil {
		t.Fatal("missing file should give no hosts")
	}
}
