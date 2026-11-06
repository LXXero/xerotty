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

// fakeLoginctl answers show-user / show-session from a table of
// sessions, in loginctl's Key=Value format.
func fakeLoginctl(t *testing.T, sessions map[string]string) {
	t.Helper()
	old := runLoginctl
	runLoginctl = func(args ...string) (string, error) {
		switch args[0] {
		case "show-user":
			ids := make([]string, 0, len(sessions))
			for id := range sessions {
				ids = append(ids, id)
			}
			return "Sessions=" + strings.Join(ids, " ") + "\n", nil
		case "show-session":
			return sessions[args[1]], nil
		}
		t.Fatalf("unexpected loginctl %v", args)
		return "", nil
	}
	t.Cleanup(func() { runLoginctl = old })
}

func TestSystemdScopeGate(t *testing.T) {
	if !userManagerReachable() {
		t.Skip("no systemd user manager here")
	}
	noLinger := t.TempDir()
	oldDir := lingerDir
	lingerDir = noLinger
	t.Cleanup(func() { lingerDir = oldDir })

	const ssh = "Remote=yes\nClass=user\nState=active\n"
	const manager = "State=active\nRemote=no\nClass=manager\n"
	const console = "Class=user\nState=active\nRemote=no\n" // property order varies: this one differs on purpose
	const consoleClosing = "Remote=no\nState=closing\nClass=user\n"
	const consoleOnline = "Remote=no\nState=online\nClass=user\n" // VT switched away, still a login

	cases := []struct {
		name     string
		sessions map[string]string
		linger   bool
		want     bool
	}{
		{"ssh only, no linger: headless box reached over ssh", map[string]string{"7": ssh, "4": manager}, false, false},
		{"closing console only: nothing will outlive the daemon", map[string]string{"3": consoleClosing, "7": ssh}, false, false},
		{"console login up: manager stays while the user is logged in", map[string]string{"3": console, "4": manager, "7": ssh}, false, true},
		{"console switched to another VT still counts", map[string]string{"3": consoleOnline}, false, true},
		{"linger on makes the scope unconditional", map[string]string{"7": ssh}, true, true},
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeLoginctl(t, tc.sessions)
			lingerFile := filepath.Join(noLinger, me.Username)
			os.Remove(lingerFile)
			if tc.linger {
				if err := os.WriteFile(lingerFile, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := systemdScope.available(); got != tc.want {
				t.Fatalf("available=%v want %v (reason: %q)", got, tc.want, systemdScope.unavailableReason())
			}
		})
	}
}
