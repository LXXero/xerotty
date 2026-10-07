package runner

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/testutil"
)

// upgradeFixture is a supervised `xerotty serve` running from an
// installed path, with one tab whose shell pid is known and whose
// scrollback holds FILLER_0..39. "Installing" a build renames a new
// file over that path, as `make build` does: a new inode at the old
// path, which is what --upgrade has to get running.
type upgradeFixture struct {
	t        *testing.T
	build    []byte // the freshly built binary, for installs
	bin      string // installed path the daemon runs from
	cli      string // separate copy that runs `serve --upgrade`
	sock     string
	mcpSock  string
	env      []string
	srv      *exec.Cmd
	tabID    uint32
	shellPID string
	instance string
}

func startUpgradeFixture(t *testing.T, serveArgs []string, env ...string) *upgradeFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a binary; skipped in -short")
	}
	tmp := t.TempDir()
	built := filepath.Join(tmp, "xerotty-build")
	build := exec.Command("go", "build", "-tags", "headless", "-o", built, "./cmd/xerotty")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("test binary build failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(built)
	if err != nil {
		t.Fatal(err)
	}
	f := &upgradeFixture{t: t, build: data, bin: filepath.Join(tmp, "xerotty"), cli: filepath.Join(tmp, "xerotty-cli")}
	f.install(data)
	if err := os.WriteFile(f.cli, data, 0o755); err != nil {
		t.Fatal(err)
	}
	sd := testutil.SockDir(t)
	f.sock = filepath.Join(sd, "d.sock")
	f.mcpSock = filepath.Join(sd, "d.mcp.sock")
	logPath := filepath.Join(tmp, "daemon.log")
	logF, _ := os.Create(logPath)
	t.Cleanup(func() { logF.Close() })

	f.env = append(append(os.Environ(), "XDG_CACHE_HOME="+tmp, "SHELL=/bin/sh", "XEROTTY_UPGRADE_TIMEOUT=10s"), env...)
	args := append(append([]string{"serve"}, serveArgs...), "--socket", f.sock, "--mcp-socket", f.mcpSock)
	f.srv = exec.Command(f.bin, args...)
	f.srv.Env = f.env
	f.srv.Stdout = logF
	f.srv.Stderr = logF
	// Own process group: cleanup must take supervisor and child.
	f.srv.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := f.srv.Start(); err != nil {
		t.Skipf("start daemon: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			_ = syscall.Kill(-f.srv.Process.Pid, syscall.SIGQUIT)
			time.Sleep(500 * time.Millisecond)
		}
		_ = syscall.Kill(-f.srv.Process.Pid, syscall.SIGKILL)
		_, _ = f.srv.Process.Wait()
		if t.Failed() {
			if b, err := os.ReadFile(logPath); err == nil {
				t.Logf("daemon log:\n%s", b)
			}
		}
	})

	for _, p := range []string{f.sock, f.mcpSock} {
		if !waitDial(p) {
			t.Fatalf("%s never came up", p)
		}
	}
	wcDone, instance := attachWireClient(t, f.sock)
	t.Cleanup(wcDone)
	f.instance = instance
	probe := f.probe()
	tabsRes, err := probe.call("tabs/list", nil)
	if err != nil {
		t.Fatal(err)
	}
	var tabs []struct {
		ID uint32 `json:"id"`
	}
	if err := json.Unmarshal(tabsRes, &tabs); err != nil || len(tabs) == 0 {
		t.Fatalf("no tabs: %v %s", err, tabsRes)
	}
	f.tabID = tabs[0].ID
	if _, err := probe.call("tab/input", map[string]any{"tab_id": f.tabID, "bytes": "echo PIDIS_$$\r"}); err != nil {
		t.Fatal(err)
	}
	pidRe := regexp.MustCompile(`PIDIS_(\d+)`)
	f.waitScreen(probe, func(scr string) bool {
		if m := pidRe.FindStringSubmatch(scr); m != nil {
			f.shellPID = m[1]
		}
		return f.shellPID != ""
	}, "shell pid never echoed")
	if _, err := probe.call("tab/input", map[string]any{"tab_id": f.tabID,
		"bytes": "i=0; while [ $i -lt 40 ]; do echo FILLER_$i; i=$((i+1)); done\r"}); err != nil {
		t.Fatal(err)
	}
	f.waitScreen(probe, func(scr string) bool { return strings.Contains(scr, "FILLER_39") }, "filler never printed")
	// Let the daemon's debounced topology push reach the supervisor.
	time.Sleep(500 * time.Millisecond)
	return f
}

func waitDial(path string) bool {
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// install renames data over the installed path and returns the new
// file's identity.
func (f *upgradeFixture) install(data []byte) fileID {
	f.t.Helper()
	tmp := f.bin + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Rename(tmp, f.bin); err != nil {
		f.t.Fatal(err)
	}
	id, err := statFile(f.bin)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

// upgrade runs `serve --upgrade` from the separate CLI copy.
func (f *upgradeFixture) upgrade() (string, error) {
	cmd := exec.Command(f.cli, "serve", "--upgrade", "--socket", f.sock)
	cmd.Env = f.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// probe returns an MCP probe once a daemon answers on the socket.
func (f *upgradeFixture) probe() *mcpProbe {
	f.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		p, err := dialProbe(f.mcpSock)
		if err == nil {
			if _, err := p.call("agent/mode", map[string]any{"mode": "auto"}); err == nil {
				return p
			}
			p.conn.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.t.Fatal("no daemon answered on the MCP socket")
	return nil
}

func (f *upgradeFixture) waitScreen(p *mcpProbe, ok func(string) bool, msg string) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var scr string
	for time.Now().Before(deadline) {
		var err error
		if scr, err = p.screen(f.tabID); err == nil && ok(scr) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.t.Fatalf("%s:\n%s", msg, scr)
}

// sameSession proves the tab survived: the same shell answers new
// input, the pre-upgrade scrollback is there, and a wire client sees
// the same daemon instance.
func (f *upgradeFixture) sameSession(marker string) {
	f.t.Helper()
	p := f.probe()
	if _, err := p.call("tab/input", map[string]any{"tab_id": f.tabID, "bytes": "echo " + marker + "_$$\r"}); err != nil {
		f.t.Fatalf("input: %v", err)
	}
	want := marker + "_" + f.shellPID
	f.waitScreen(p, func(scr string) bool { return strings.Contains(scr, want) }, "same shell did not answer ("+want+")")
	sb, err := p.call("tab/scrollback", map[string]any{"tab_id": f.tabID, "lines": 100})
	if err != nil {
		f.t.Fatalf("tab/scrollback: %v", err)
	}
	if !strings.Contains(string(sb), "FILLER_0") {
		f.t.Fatalf("scrollback lost: %.300s", sb)
	}
	done, instance := attachWireClient(f.t, f.sock)
	done()
	if instance != f.instance {
		f.t.Fatalf("InstanceID changed: %q -> %q", f.instance, instance)
	}
}

func mustMap(t *testing.T, pid int, id fileID, want bool, who string) {
	t.Helper()
	ok, mapped, err := mapsFile(pid, id)
	if err != nil {
		t.Fatalf("%s %d: maps: %v", who, pid, err)
	}
	if ok != want {
		t.Fatalf("%s %d maps target inode %d = %v, want %v (first mapping: %s)", who, pid, id.ino, ok, want, mapped)
	}
}

// TestSupervisedUpgradeE2E: `serve --upgrade` against a supervisor.
// A build the gate rejects must leave everything running and be
// reported as a failure; a real new build must end with a new daemon
// child AND the supervisor itself mapping the new inode, the same
// shell in the same tab, scrollback and screen intact.
func TestSupervisedUpgradeE2E(t *testing.T) {
	f := startUpgradeFixture(t, nil)
	sup := f.srv.Process.Pid
	child := childOf(t, sup)
	oldID, err := statFile(f.bin)
	if err != nil {
		t.Fatal(err)
	}

	// ---- A broken install: the supervisor's validation gate refuses
	// it, nothing changes, and the CLI must not claim otherwise.
	f.install([]byte("#!/bin/sh\nexit 1\n"))
	out, err := f.upgrade()
	if err == nil || strings.Contains(out, "upgraded") || !strings.Contains(out, "FAILED") {
		t.Fatalf("upgrade to a broken build must fail loudly (err %v):\n%s", err, out)
	}
	if c := childOf(t, sup); c != child {
		t.Fatalf("daemon child changed %d -> %d on a rejected upgrade", child, c)
	}
	mustMap(t, child, oldID, true, "daemon child")

	// ---- The real install.
	newID := f.install(f.build)
	out, err = f.upgrade()
	if err != nil {
		t.Fatalf("serve --upgrade: %v\n%s", err, out)
	}
	if !regexp.MustCompile(`upgraded: supervisor \d+ and daemon child \d+ run `).MatchString(out) {
		t.Fatalf("no success line:\n%s", out)
	}
	newChild := childOf(t, sup)
	if newChild == child {
		t.Fatalf("daemon child %d was not replaced:\n%s", child, out)
	}
	mustMap(t, newChild, newID, true, "daemon child")
	mustMap(t, sup, newID, true, "supervisor")
	mustMap(t, sup, oldID, false, "supervisor")

	// The handoff carried the screen, not just the shell: the last
	// filler line is back before anything new is typed (a crash
	// resume would show a blank screen and a fresh prompt).
	p := f.probe()
	f.waitScreen(p, func(scr string) bool { return strings.Contains(scr, "FILLER_39") }, "screen lost across the upgrade")
	f.sameSession("AFTER")
}

// TestUpgradeOldSupervisorFallbackE2E: a supervisor that predates
// SIGUSR2 upgrades (every host before this change) gets its child
// SIGKILLed, restarts it from the new binary on disk through the
// crash-resume path, and the CLI says that is what it did.
func TestUpgradeOldSupervisorFallbackE2E(t *testing.T) {
	f := startUpgradeFixture(t, nil, "XEROTTY_TEST_LEGACY_SUPERVISOR=1")
	sup := f.srv.Process.Pid
	child := childOf(t, sup)
	oldID, err := statFile(f.bin)
	if err != nil {
		t.Fatal(err)
	}
	newID := f.install(f.build)
	out, err := f.upgrade()
	if err != nil {
		t.Fatalf("serve --upgrade: %v\n%s", err, out)
	}
	for _, want := range []string{"predates in-place upgrades", "SIGKILLing its daemon child", "still runs its old code"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	newChild := childOf(t, sup)
	if newChild == child {
		t.Fatalf("daemon child %d was not replaced:\n%s", child, out)
	}
	mustMap(t, newChild, newID, true, "daemon child")
	mustMap(t, sup, oldID, true, "supervisor") // can't upgrade itself
	f.sameSession("AFTER")
}
