package runner

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/testutil"
)

// binaryReplaced gates the bridge's exec-in-place self-upgrade, so it
// must fire on a real change and never on a missing snapshot/file
// (which would risk exec'ing into nothing).
func TestBinaryReplaced(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "xerotty")
	if err := os.WriteFile(p, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	start, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	if binaryReplaced(start, p) {
		t.Error("unchanged binary reported as replaced")
	}
	if binaryReplaced(nil, p) {
		t.Error("nil snapshot must be false (no baseline to compare)")
	}
	if binaryReplaced(start, filepath.Join(dir, "missing")) {
		t.Error("missing file must be false (don't exec into nothing)")
	}

	// Size change → replaced.
	if err := os.WriteFile(p, []byte("v2-larger"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !binaryReplaced(start, p) {
		t.Error("size change not detected")
	}

	// Same size, newer mtime → replaced.
	if err := os.WriteFile(p, []byte("v3"), 0o755); err != nil {
		t.Fatal(err)
	}
	snap, _ := os.Stat(p)
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	if !binaryReplaced(snap, p) {
		t.Error("mtime change not detected")
	}
}

// A bridge re-exec'd with flavor "gui" while only the daemon socket is
// up must not attach to the daemon (the client's tab_id schema would
// flip from strings to integers mid-session). It waits within the
// reconnect window and attaches once a GUI socket appears.
func TestReexecKeepsFlavorWaitsForSameKind(t *testing.T) {
	dir := testutil.SockDir(t)
	guiSock := filepath.Join(dir, "gui-mcp.sock")
	daemonSock := filepath.Join(dir, "daemon-mcp.sock")

	dln, err := net.Listen("unix", daemonSock)
	if err != nil {
		t.Fatalf("listen daemon: %v", err)
	}
	defer dln.Close()
	var daemonAccepts atomic.Int32
	go func() {
		for {
			c, err := dln.Accept()
			if err != nil {
				return
			}
			daemonAccepts.Add(1)
			c.Close()
		}
	}()

	t.Setenv(mcpReplayModeEnv, "")
	t.Setenv(mcpFlavorEnv, "gui")
	if !isContinuation() {
		t.Fatal("inherited flavor must mark the process as a continuation")
	}
	locked := make(chan string, 4)
	candidates := func() []mcpCandidate {
		return []mcpCandidate{{guiSock, "gui"}, {daemonSock, "daemon"}}
	}
	discover := newMCPDiscover(candidates, inheritedFlavor(), func(kind string) {
		locked <- kind
	})

	pr, pw := io.Pipe()
	var out bytes.Buffer
	exit := make(chan int, 1)
	go func() { exit <- runBridge(discover, pr, &out) }()
	fmt.Fprintln(pw, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	time.Sleep(600 * time.Millisecond)
	select {
	case code := <-exit:
		t.Fatalf("bridge exited %d instead of waiting for a GUI socket", code)
	default:
	}
	if n := daemonAccepts.Load(); n != 0 {
		t.Fatalf("bridge dialed the daemon socket %d times while locked to gui", n)
	}

	gln, err := net.Listen("unix", guiSock)
	if err != nil {
		t.Fatalf("listen gui: %v", err)
	}
	defer gln.Close()
	echoServer(t, gln, 0)

	select {
	case kind := <-locked:
		if kind != "gui" {
			t.Fatalf("locked onto %q, want gui", kind)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bridge never attached to the GUI socket")
	}
	time.Sleep(200 * time.Millisecond) // let the buffered request's response land
	pw.Close()
	if code := <-exit; code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(locked) != 0 {
		t.Fatalf("bridge locked again: %q", <-locked)
	}
	if n := daemonAccepts.Load(); n != 0 {
		t.Fatalf("daemon socket was dialed %d times", n)
	}
	if !strings.Contains(out.String(), `"n":1`) {
		t.Fatalf("buffered request was not served by the GUI socket: %q", out.String())
	}
}

// With no inherited flavor, discovery takes the first live socket in
// order (the cold-launch behavior) and then stays on that kind.
func TestDiscoverLocksFirstLiveKind(t *testing.T) {
	dir := testutil.SockDir(t)
	guiSock := filepath.Join(dir, "gui-mcp.sock")
	daemonSock := filepath.Join(dir, "daemon-mcp.sock")
	dln, err := net.Listen("unix", daemonSock)
	if err != nil {
		t.Fatalf("listen daemon: %v", err)
	}
	defer dln.Close()
	echoServer(t, dln, 0)

	var locked string
	discover := newMCPDiscover(func() []mcpCandidate {
		return []mcpCandidate{{guiSock, "gui"}, {daemonSock, "daemon"}}
	}, "", func(kind string) { locked = kind })
	c := discover()
	if c == nil || locked != "daemon" {
		t.Fatalf("cold discover: conn=%v locked=%q, want daemon", c, locked)
	}
	c.Close()

	// A GUI socket appearing later must not pull the bridge over.
	gln, err := net.Listen("unix", guiSock)
	if err != nil {
		t.Fatalf("listen gui: %v", err)
	}
	defer gln.Close()
	echoServer(t, gln, 0)
	c = discover()
	if c == nil || locked != "daemon" {
		t.Fatalf("redial switched kind: locked=%q", locked)
	}
	c.Close()
}

func TestInheritedFlavorIgnoresUnknown(t *testing.T) {
	for in, want := range map[string]string{"gui": "gui", "daemon": "daemon", "explicit": "explicit", "": "", "bogus": ""} {
		t.Setenv(mcpFlavorEnv, in)
		if got := inheritedFlavor(); got != want {
			t.Errorf("inheritedFlavor(%q) = %q, want %q", in, got, want)
		}
	}
}
