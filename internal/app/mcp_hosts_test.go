package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/config"
	"github.com/LXXero/xerotty/internal/protocol"
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

func TestAttachHostDialsThroughTheRemoteMenuPath(t *testing.T) {
	sockPath := startLocalDaemon(t)
	a, w := newDialTestApp(t)
	a.active = w
	dials := 0
	dialer := localDialer(a, sockPath)
	a.remoteDialer = func(name string, h config.RemoteHost, cancel <-chan struct{}) (*clientproto.Client, *protocol.Attached, error) {
		dials++
		return dialer(name, h, cancel)
	}

	var already bool
	var err error
	done := make(chan struct{})
	go func() {
		already, err = a.AttachHost(context.Background(), "xhtpc")
		close(done)
	}()
	runFrames(t, a, done)
	if err != nil || already {
		t.Fatalf("AttachHost = already %v, err %v; want a fresh attach", already, err)
	}
	if a.lookupRemoteHub("xhtpc") == nil {
		t.Fatal("hub not registered under the host name")
	}
	if !slices.Contains(a.EventHosts(), "xhtpc") {
		t.Fatalf("attached host missing from the host namespaces: %v", a.EventHosts())
	}
	if w.connectingHost || w.connectPending {
		t.Fatal("connect dialog left open after the attach")
	}
	// The daemon had no tabs: an MCP attach opens none of its own.
	if len(w.daemonWindowIDs) != 0 {
		t.Fatal("attach created a daemon window for a tab")
	}

	// Attached now: answered without the render thread or a dial.
	already, err = a.AttachHost(context.Background(), "xhtpc")
	if err != nil || !already || dials != 1 {
		t.Fatalf("second AttachHost = already %v, err %v, dials %d", already, err, dials)
	}
}

func TestAttachHostReportsDialFailure(t *testing.T) {
	a, w := newDialTestApp(t)
	a.active = w
	a.remoteDialer = func(string, config.RemoteHost, <-chan struct{}) (*clientproto.Client, *protocol.Attached, error) {
		return nil, nil, errors.New("ssh: Could not resolve hostname nope")
	}
	var err error
	done := make(chan struct{})
	go func() {
		_, err = a.AttachHost(context.Background(), "nope")
		close(done)
	}()
	runFrames(t, a, done)
	if err == nil || !strings.Contains(err.Error(), "Could not resolve hostname") {
		t.Fatalf("AttachHost err = %v", err)
	}
	// Same as a failed manual attach: the dialog shows the error.
	if !w.connectingHost || !strings.Contains(w.connectError, "Could not resolve hostname") {
		t.Fatalf("dialog: open=%v err=%q", w.connectingHost, w.connectError)
	}
}

func TestSSHConfigHosts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config")
	cfg := "Host *\n  ServerAliveInterval 60\nHost kh zx # two names\n  HostName kh.example\n" +
		"host=xwork\nHost !bad 192.168.1.1 web?\nMatch host foo\nHost kh\n"
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	got := sshConfigHosts(p)
	want := []string{"kh", "zx", "xwork", "192.168.1.1"}
	if !slices.Equal(got, want) {
		t.Fatalf("sshConfigHosts = %v, want %v", got, want)
	}
	if sshConfigHosts(filepath.Join(t.TempDir(), "missing")) != nil {
		t.Fatal("missing file should give no hosts")
	}
}
