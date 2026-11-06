//go:build !windows

package daemonspawn

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/testutil"
)

func TestDaemonEnvDropsSessionIdentityOnly(t *testing.T) {
	in := []string{
		"HOME=/home/x",
		"SSH_CLIENT=192.168.1.10 48814 22",
		"SSH_CONNECTION=a b c d",
		"SSH_TTY=/dev/pts/3",
		"SSH_AUTH_SOCK=/tmp/agent.sock", // functional, must survive
		"XDG_SESSION_ID=3",
		"XDG_SESSION_TYPE=tty",
		"XDG_RUNTIME_DIR=/run/user/1000", // not session identity
		"XDG_SEAT=seat0",
		"XDG_VTNR=1",
		"XDG_SESSION_IDX=weird", // prefix match must be on NAME=
		"PATH=/usr/bin",
	}
	got := strings.Join(DaemonEnv(in), "\n")
	for _, keep := range []string{"HOME=", "SSH_AUTH_SOCK=", "XDG_RUNTIME_DIR=", "XDG_SESSION_IDX=", "PATH="} {
		if !strings.Contains(got, keep) {
			t.Errorf("dropped %s, should keep", keep)
		}
	}
	for _, drop := range []string{"SSH_CLIENT=", "SSH_CONNECTION=", "SSH_TTY=", "XDG_SESSION_ID=", "XDG_SESSION_TYPE=", "XDG_SEAT=", "XDG_VTNR="} {
		if strings.Contains(got, drop) {
			t.Errorf("kept %s, should drop", drop)
		}
	}
}

func TestUnitNameStablePrefixDistinctSuffix(t *testing.T) {
	a := UnitName("/run/user/1000/xerottyd.sock")
	time.Sleep(2 * time.Millisecond)
	b := UnitName("/run/user/1000/xerottyd.sock")
	c := UnitName("/tmp/other.sock")
	prefix := func(s string) string { return s[:strings.LastIndex(s, "-")] }
	if prefix(a) != prefix(b) {
		t.Fatalf("same socket, different stem: %s vs %s", a, b)
	}
	if a == b {
		t.Fatalf("two spawns got the same unit name %s", a)
	}
	if prefix(a) == prefix(c) {
		t.Fatalf("different sockets share a stem: %s", prefix(a))
	}
	if !strings.HasPrefix(a, "xerotty-daemon-") {
		t.Fatalf("unit name %q lacks the listing prefix", a)
	}
}

// fakeDaemon is a stand-in for `xerotty serve --socket <path>`: a
// script that binds the socket (so Dial succeeds) and idles.
func fakeDaemon(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 needed to bind a unix socket")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "fake-xerotty")
	script := `#!/bin/sh
# argv: serve --socket <path>
exec python3 -c 'import socket,sys,time
s=socket.socket(socket.AF_UNIX); s.bind(sys.argv[1]); s.listen(1); time.sleep(60)' "$3"
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnsureListeningFallsThroughOnEarlyExit(t *testing.T) {
	sock := filepath.Join(testutil.SockDir(t), "d.sock")
	self := fakeDaemon(t)
	failing := strategy{
		name:      "exits-at-once",
		available: func() bool { return true },
		start: func(o Options, env []string) (handle, error) {
			cmd := exec.Command("sh", "-c", "exit 3")
			if err := cmd.Start(); err != nil {
				return handle{}, err
			}
			return handle{exited: waitExit(cmd)}, nil
		},
	}
	unavailable := strategy{
		name:      "never",
		available: func() bool { return false },
		start: func(Options, []string) (handle, error) {
			t.Fatal("unavailable strategy must not be started")
			return handle{}, nil
		},
	}
	var pid int
	fork := forkStrategy
	fork.start = func(o Options, env []string) (handle, error) {
		h, err := forkStrategy.start(o, env)
		pid = h.pid
		return h, err
	}
	name, err := ensureListening(Options{Self: self, SocketPath: sock, Dial: dialer(sock), Timeout: 5 * time.Second},
		[]strategy{unavailable, failing, fork})
	if err != nil {
		t.Fatal(err)
	}
	if name != "fork" {
		t.Fatalf("strategy = %q, want fork", name)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	// setsid: the daemon leads its own session.
	if sid := sessionID(t, pid); sid != pid {
		t.Fatalf("daemon sid=%d pid=%d; want its own session", sid, pid)
	}
}

func TestEnsureListeningTimeoutAborts(t *testing.T) {
	sock := filepath.Join(testutil.SockDir(t), "never.sock")
	var pid int
	stuck := strategy{
		name:      "stuck",
		available: func() bool { return true },
		start: func(o Options, env []string) (handle, error) {
			cmd := exec.Command("sleep", "30")
			if err := cmd.Start(); err != nil {
				return handle{}, err
			}
			pid = cmd.Process.Pid
			return handle{exited: waitExit(cmd), abort: func() { _ = cmd.Process.Kill() }}, nil
		},
	}
	start := time.Now()
	_, err := ensureListening(Options{Self: "x", SocketPath: sock, Dial: dialer(sock), Timeout: 300 * time.Millisecond},
		[]strategy{stuck, forkStrategy})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %s; timeout not honoured", d)
	}
	// abort must have killed it; give the kernel a moment.
	time.Sleep(50 * time.Millisecond)
	if err := syscall.Kill(pid, 0); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("stuck process %d still alive after abort", pid)
	}
}
