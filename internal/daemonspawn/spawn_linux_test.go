package daemonspawn

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/testutil"
)

// TestRealStrategyPlacement runs the platform's first-choice strategy
// against a fake daemon and checks where the process ended up. On
// Linux with a systemd user manager that is: inside
// user@<uid>.service, in a scope carrying our unit name, outside any
// session scope. Skips where the strategy isn't available.
func TestRealStrategyPlacement(t *testing.T) {
	all := strategies()
	first := all[0]
	if first.name != "systemd user scope" {
		t.Skipf("no placement check written for %q", first.name)
	}
	// The linger gate is about lifetime, not placement; exercise the
	// mechanism whenever a user manager is reachable.
	if !userManagerReachable() {
		t.Skip("no systemd user manager here")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	probe := exec.CommandContext(ctx, "systemd-run", "--user", "--scope", "true")
	if output, err := probe.CombinedOutput(); err != nil {
		t.Skipf("systemd user scope is not usable: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	sock := filepath.Join(testutil.SockDir(t), "p.sock")
	// SockDir redirects the runtime directory for ordinary socket tests;
	// this placement test needs the real user manager's bus.
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	self := fakeDaemon(t)
	var pid int
	s := first
	s.available = func() bool { return true } // placement test, not the linger gate
	s.unavailableReason = nil
	s.start = func(o Options, env []string) (handle, error) {
		h, err := first.start(o, env)
		pid = h.pid
		return h, err
	}
	logf, _ := os.CreateTemp(t.TempDir(), "log")
	name, err := ensureListening(Options{Self: self, SocketPath: sock, Dial: dialer(sock), Timeout: 5 * time.Second, Stderr: logf},
		[]strategy{s})
	if err != nil {
		b, _ := os.ReadFile(logf.Name())
		t.Fatalf("%v\nlog:\n%s", err, b)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if name != first.name {
		t.Fatalf("strategy = %q", name)
	}
	cg, err := os.ReadFile("/proc/" + itoa(pid) + "/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	cgroup := strings.TrimSpace(string(cg))
	if !strings.Contains(cgroup, "/user@") || !strings.Contains(cgroup, "xerotty-daemon-") || strings.Contains(cgroup, "/session-") {
		t.Fatalf("daemon cgroup %q: want under user@<uid>.service in a xerotty-daemon-* scope, not a session scope", cgroup)
	}
	// And the session-identity variables are gone from its env.
	envb, err := os.ReadFile("/proc/" + itoa(pid) + "/environ")
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range strings.Split(string(envb), "\x00") {
		for _, k := range sessionIdentity {
			if strings.HasPrefix(kv, k+"=") {
				t.Errorf("daemon env still has %s", kv)
			}
		}
	}
}

func TestSystemdScopeRequiresLinger(t *testing.T) {
	if !userManagerReachable() {
		t.Skip("no systemd user manager here")
	}
	dir := t.TempDir()
	old := lingerDir
	lingerDir = dir
	t.Cleanup(func() { lingerDir = old })
	if systemdScope.available() {
		t.Fatal("scope strategy available with no linger file; a logout would kill the daemon")
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, me.Username), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !systemdScope.available() {
		t.Fatal("scope strategy unavailable although linger is enabled")
	}
}
