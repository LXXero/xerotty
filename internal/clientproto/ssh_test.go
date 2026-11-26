package clientproto

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The destination always follows "--", so ssh never reads it as an
// option, and the user's own options stay in front.
func TestSSHArgsEndOptionsBeforeDest(t *testing.T) {
	args := sshArgs("-oProxyCommand=touch x", "", []string{"-p", "2222"})
	i := slices.Index(args, "--")
	if i < 0 || i+2 != len(args)-1 || args[i+1] != "-oProxyCommand=touch x" || args[i+2] != "xerotty serve --stdio" {
		t.Fatalf("sshArgs = %q; want ... -- <dest> <cmd>", args)
	}
	if args[0] != "-p" || args[1] != "2222" {
		t.Fatalf("extra ssh args not first: %q", args)
	}
}

func TestValidSSHDest(t *testing.T) {
	for _, ok := range []string{"xhtpc", "xero@xhtpc", "xhtpc:2222", "kh.zaxxon.cc", "192.168.1.1", "a-b_c",
		"h:65535", "2001:db8::1", "xero@2001:db8::1", "::ffff:192.168.1.1", "::1"} {
		if err := ValidSSHDest(ok); err != nil {
			t.Errorf("ValidSSHDest(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{"", "-oProxyCommand=x", "-p", "a b", "a\tb", "x=y", "h;id", "h$(id)", "h`id`",
		"h|id", "h&", "h>f", "'h'", "a@b@c", "user@", "user@-x", "ssh://h", "h/x", "h%d", "h\n",
		"@h", "-u@h", "h:", "h:0", "h:01", "h:65536", "h:+1", "h:2x", ":22", "[::1]:22", "[::1]", "fe80::1%eth0", "h:2:3",
		":::", "1:2:3:4:5:6:7:8:9", "::g"} {
		if err := ValidSSHDest(bad); err == nil {
			t.Errorf("ValidSSHDest(%q) accepted", bad)
		}
	}
}

// Close returns even when a child of the transport, as a ProxyCommand
// helper would be, keeps its stderr open after the transport exits.
// Without a bound on Wait, Close waited for that child.
func TestCloseReturnsWhileAGrandchildHoldsStderr(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	c, err := DialCommand("sh", "-c", "sleep 30 >&2 & echo $! >"+pidFile+"; exec cat >/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(pidFile); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	done := make(chan struct{})
	go func() { _ = c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close still waiting on the grandchild's stderr after 10s")
	}
}
